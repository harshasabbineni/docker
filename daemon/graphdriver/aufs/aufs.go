// +build linux

/*

aufs driver directory structure

  .
  ├── layers // Metadata of layers
  │   ├── 1
  │   ├── 2
  │   └── 3
  ├── diff  // Content of the layer
  │   ├── 1  // Contains layers that need to be mounted for the id
  │   ├── 2
  │   └── 3
  └── mnt    // Mount points for the rw layers to be mounted
      ├── 1
      ├── 2
      └── 3

*/

package aufs

import (
	"bufio"
	"fmt"
	"io/ioutil"
	"os"
	"os/exec"
	"path"
	"path/filepath"
	"strings"
	"sync"
	"syscall"

	"github.com/Sirupsen/logrus"
	"github.com/vbatts/tar-split/tar/storage"

	"github.com/docker/docker/daemon/graphdriver"
	"github.com/docker/docker/pkg/archive"
	"github.com/docker/docker/pkg/chrootarchive"
	"github.com/docker/docker/pkg/directory"
	"github.com/docker/docker/pkg/idtools"
	mountpk "github.com/docker/docker/pkg/mount"
	"github.com/docker/docker/pkg/stringid"

	"github.com/opencontainers/runc/libcontainer/label"
)

var (
	// ErrAufsNotSupported is returned if aufs is not supported by the host.
	ErrAufsNotSupported = fmt.Errorf("AUFS was not found in /proc/filesystems")
	incompatibleFsMagic = []graphdriver.FsMagic{
		graphdriver.FsMagicBtrfs,
		graphdriver.FsMagicAufs,
	}
	backingFs = "<unknown>"

	enableDirpermLock sync.Once
	enableDirperm     bool
)

func init() {
	graphdriver.Register("aufs", Init)
}

// Driver contains information about the filesystem mounted.
type Driver struct {
	sync.Mutex
	root          string
	uidMaps       []idtools.IDMap
	gidMaps       []idtools.IDMap
	pathCacheLock sync.Mutex
	pathCache     map[string]string
}

// Init returns a new AUFS driver.
// An error is returned if AUFS is not supported.
func Init(root string, options []string, uidMaps, gidMaps []idtools.IDMap) (graphdriver.Driver, error) {

	// Try to load the aufs kernel module
	if err := supportsAufs(); err != nil {
		return nil, graphdriver.ErrNotSupported
	}

	fsMagic, err := graphdriver.GetFSMagic(root)
	if err != nil {
		return nil, err
	}
	if fsName, ok := graphdriver.FsNames[fsMagic]; ok {
		backingFs = fsName
	}

	for _, magic := range incompatibleFsMagic {
		if fsMagic == magic {
			return nil, graphdriver.ErrIncompatibleFS
		}
	}

	paths := []string{
		"mnt",
		"diff",
		"layers",
	}

	a := &Driver{
		root:      root,
		uidMaps:   uidMaps,
		gidMaps:   gidMaps,
		pathCache: make(map[string]string),
	}

	rootUID, rootGID, err := idtools.GetRootUIDGID(uidMaps, gidMaps)
	if err != nil {
		return nil, err
	}
	// Create the root aufs driver dir and return
	// if it already exists
	// If not populate the dir structure
	if err := idtools.MkdirAllAs(root, 0700, rootUID, rootGID); err != nil {
		if os.IsExist(err) {
			return a, nil
		}
		return nil, err
	}

	if err := mountpk.MakePrivate(root); err != nil {
		return nil, err
	}

	// Populate the dir structure
	for _, p := range paths {
		if err := idtools.MkdirAllAs(path.Join(root, p), 0700, rootUID, rootGID); err != nil {
			return nil, err
		}
	}
	return a, nil
}

// Return a nil error if the kernel supports aufs
// We cannot modprobe because inside dind modprobe fails
// to run
func supportsAufs() error {
	// We can try to modprobe aufs first before looking at
	// proc/filesystems for when aufs is supported
	exec.Command("modprobe", "aufs").Run()

	f, err := os.Open("/proc/filesystems")
	if err != nil {
		return err
	}
	defer f.Close()

	s := bufio.NewScanner(f)
	for s.Scan() {
		if strings.Contains(s.Text(), "aufs") {
			return nil
		}
	}
	return ErrAufsNotSupported
}

func (a *Driver) rootPath() string {
	return a.root
}

func (*Driver) String() string {
	return "aufs"
}

// Status returns current information about the filesystem such as root directory, number of directories mounted, etc.
func (a *Driver) Status() [][2]string {
	ids, _ := loadIds(path.Join(a.rootPath(), "layers"))
	return [][2]string{
		{"Root Dir", a.rootPath()},
		{"Backing Filesystem", backingFs},
		{"Dirs", fmt.Sprintf("%d", len(ids))},
		{"Dirperm1 Supported", fmt.Sprintf("%v", useDirperm())},
	}
}

// GetMetadata not implemented
func (a *Driver) GetMetadata(id string) (map[string]string, error) {
	return nil, nil
}

// Exists returns true if the given id is registered with
// this driver
func (a *Driver) Exists(id string) bool {
	if _, err := os.Lstat(path.Join(a.rootPath(), "layers", id)); err != nil {
		return false
	}
	return true
}

// CreateReadWrite creates a layer that is writable for use as a container
// file system.
func (a *Driver) CreateReadWrite(id, parent, mountLabel string, storageOpt map[string]string) error {
	return a.Create(id, parent, mountLabel, storageOpt)
}

// Create three folders for each id
// mnt, layers, and diff
func (a *Driver) Create(id, parent, mountLabel string, storageOpt map[string]string) error {

	if len(storageOpt) != 0 {
		return fmt.Errorf("--storage-opt is not supported for aufs")
	}

	if err := a.createDirsFor(id); err != nil {
		return err
	}
	// Write the layers metadata
	f, err := os.Create(path.Join(a.rootPath(), "layers", id))
	if err != nil {
		return err
	}
	defer f.Close()

	if parent != "" {
		ids, err := getParentIds(a.rootPath(), parent)
		if err != nil {
			return err
		}

		if _, err := fmt.Fprintln(f, parent); err != nil {
			return err
		}
		for _, i := range ids {
			if _, err := fmt.Fprintln(f, i); err != nil {
				return err
			}
		}
	}

	return nil
}

// createDirsFor creates two directories for the given id.
// mnt and diff
func (a *Driver) createDirsFor(id string) error {
	paths := []string{
		"mnt",
		"diff",
	}

	rootUID, rootGID, err := idtools.GetRootUIDGID(a.uidMaps, a.gidMaps)
	if err != nil {
		return err
	}
	// Directory permission is 0755.
	// The path of directories are <aufs_root_path>/mnt/<image_id>
	// and <aufs_root_path>/diff/<image_id>
	for _, p := range paths {
		if err := idtools.MkdirAllAs(path.Join(a.rootPath(), p, id), 0755, rootUID, rootGID); err != nil {
			return err
		}
	}
	return nil
}

// Remove will unmount and remove the given id.
func (a *Driver) Remove(id string) error {
	a.pathCacheLock.Lock()
	mountpoint, exists := a.pathCache[id]
	a.pathCacheLock.Unlock()
	if !exists {
		mountpoint = a.getMountpoint(id)
	}
	if err := a.unmount(mountpoint); err != nil {
		// no need to return here, we can still try to remove since the `Rename` will fail below if still mounted
		logrus.Debugf("aufs: error while unmounting %s: %v", mountpoint, err)
	}

	// Atomically remove each directory in turn by first moving it out of the
	// way (so that docker doesn't find it anymore) before doing removal of
	// the whole tree.
	tmpMntPath := path.Join(a.mntPath(), fmt.Sprintf("%s-removing", id))
	if err := os.Rename(mountpoint, tmpMntPath); err != nil && !os.IsNotExist(err) {
		return err
	}
	defer os.RemoveAll(tmpMntPath)

	tmpDiffpath := path.Join(a.diffPath(), fmt.Sprintf("%s-removing", id))
	if err := os.Rename(a.getDiffPath(id), tmpDiffpath); err != nil && !os.IsNotExist(err) {
		return err
	}
	defer os.RemoveAll(tmpDiffpath)

	// Remove the layers file for the id
	if err := os.Remove(path.Join(a.rootPath(), "layers", id)); err != nil && !os.IsNotExist(err) {
		return err
	}

	a.pathCacheLock.Lock()
	delete(a.pathCache, id)
	a.pathCacheLock.Unlock()
	return nil
}

// Get returns the rootfs path for the id.
// This will mount the dir at it's given path
func (a *Driver) Get(id, mountLabel string) (string, error) {
	parents, err := a.getParentLayerPaths(id)
	if err != nil && !os.IsNotExist(err) {
		return "", err
	}

	a.pathCacheLock.Lock()
	m, exists := a.pathCache[id]
	a.pathCacheLock.Unlock()

	if !exists {
		m = a.getDiffPath(id)
		if len(parents) > 0 {
			m = a.getMountpoint(id)
		}
	}

	// If a dir does not have a parent ( no layers )do not try to mount
	// just return the diff path to the data
	if len(parents) > 0 {
		if err := a.mount(id, m, mountLabel, parents); err != nil {
			return "", err
		}
	}

	a.pathCacheLock.Lock()
	a.pathCache[id] = m
	a.pathCacheLock.Unlock()
	return m, nil
}

// Put unmounts and updates list of active mounts.
func (a *Driver) Put(id string) error {
	a.pathCacheLock.Lock()
	m, exists := a.pathCache[id]
	if !exists {
		m = a.getMountpoint(id)
		a.pathCache[id] = m
	}
	a.pathCacheLock.Unlock()

	err := a.unmount(m)
	if err != nil {
		logrus.Debugf("Failed to unmount %s aufs: %v", id, err)
	}
	return err
}

// Diff produces an archive of the changes between the specified
// layer and its parent layer which may be "".
func (a *Driver) Diff(id, parent string) (archive.Archive, error) {
	// AUFS doesn't need the parent layer to produce a diff.
	return archive.TarWithOptions(path.Join(a.rootPath(), "diff", id), &archive.TarOptions{
		Compression:     archive.Uncompressed,
		ExcludePatterns: []string{archive.WhiteoutMetaPrefix + "*", "!" + archive.WhiteoutOpaqueDir},
		UIDMaps:         a.uidMaps,
		GIDMaps:         a.gidMaps,
	})
}

type fileGetNilCloser struct {
	storage.FileGetter
}

func (f fileGetNilCloser) Close() error {
	return nil
}

// DiffGetter returns a FileGetCloser that can read files from the directory that
// contains files for the layer differences. Used for direct access for tar-split.
func (a *Driver) DiffGetter(id string) (graphdriver.FileGetCloser, error) {
	p := path.Join(a.rootPath(), "diff", id)
	return fileGetNilCloser{storage.NewPathFileGetter(p)}, nil
}

func (a *Driver) applyDiff(id string, diff archive.Reader) error {
	return chrootarchive.UntarUncompressed(diff, path.Join(a.rootPath(), "diff", id), &archive.TarOptions{
		UIDMaps: a.uidMaps,
		GIDMaps: a.gidMaps,
	})
}

// DiffSize calculates the changes between the specified id
// and its parent and returns the size in bytes of the changes
// relative to its base filesystem directory.
func (a *Driver) DiffSize(id, parent string) (size int64, err error) {
	// AUFS doesn't need the parent layer to calculate the diff size.
	return directory.Size(path.Join(a.rootPath(), "diff", id))
}

// ApplyDiff extracts the changeset from the given diff into the
// layer with the specified id and parent, returning the size of the
// new layer in bytes.
func (a *Driver) ApplyDiff(id, parent string, diff archive.Reader) (size int64, err error) {
	// AUFS doesn't need the parent id to apply the diff.
	if err = a.applyDiff(id, diff); err != nil {
		return
	}

	return a.DiffSize(id, parent)
}

// Changes produces a list of changes between the specified layer
// and its parent layer. If parent is "", then all changes will be ADD changes.
func (a *Driver) Changes(id, parent string) ([]archive.Change, error) {
	// AUFS doesn't have snapshots, so we need to get changes from all parent
	// layers.
	layers, err := a.getParentLayerPaths(id)
	if err != nil {
		return nil, err
	}
	return archive.Changes(layers, path.Join(a.rootPath(), "diff", id))
}

func (a *Driver) getParentLayerPaths(id string) ([]string, error) {
	parentIds, err := getParentIds(a.rootPath(), id)
	if err != nil {
		return nil, err
	}
	layers := make([]string, len(parentIds))

	// Get the diff paths for all the parent ids
	for i, p := range parentIds {
		layers[i] = path.Join(a.rootPath(), "diff", p)
	}
	return layers, nil
}

func (a *Driver) mount(id string, target string, mountLabel string, layers []string) error {
	a.Lock()
	defer a.Unlock()

	// If the id is mounted or we get an error return
	if mounted, err := a.mounted(target); err != nil || mounted {
		return err
	}

	rw := a.getDiffPath(id)

	if err := a.aufsMount(layers, rw, target, mountLabel); err != nil {
		return fmt.Errorf("error creating aufs mount to %s: %v", target, err)
	}
	return nil
}

func (a *Driver) unmount(mountPath string) error {
	a.Lock()
	defer a.Unlock()

	if mounted, err := a.mounted(mountPath); err != nil || !mounted {
		return err
	}
	if err := Unmount(mountPath); err != nil {
		return err
	}
	return nil
}

func (a *Driver) mounted(mountpoint string) (bool, error) {
	return graphdriver.Mounted(graphdriver.FsMagicAufs, mountpoint)
}

// Cleanup aufs and unmount all mountpoints
func (a *Driver) Cleanup() error {
	var dirs []string
	if err := filepath.Walk(a.mntPath(), func(path string, info os.FileInfo, err error) error {
		if err != nil {
			return err
		}
		if !info.IsDir() {
			return nil
		}
		dirs = append(dirs, path)
		return nil
	}); err != nil {
		return err
	}

	for _, m := range dirs {
		if err := a.unmount(m); err != nil {
			logrus.Debugf("aufs error unmounting %s: %s", stringid.TruncateID(m), err)
		}
	}
	return mountpk.Unmount(a.root)
}

func (a *Driver) aufsMount(ro []string, rw, target, mountLabel string) (err error) {
	defer func() {
		if err != nil {
			Unmount(target)
		}
	}()

	// Mount options are clipped to page size(4096 bytes). If there are more
	// layers then these are remounted individually using append.

	offset := 54
	if useDirperm() {
		offset += len("dirperm1")
	}
	b := make([]byte, syscall.Getpagesize()-len(mountLabel)-offset) // room for xino & mountLabel
	bp := copy(b, fmt.Sprintf("br:%s=rw", rw))

	firstMount := true
	i := 0

	for {
		for ; i < len(ro); i++ {
			layer := fmt.Sprintf(":%s=ro+wh", ro[i])

			if firstMount {
				if bp+len(layer) > len(b) {
					break
				}
				bp += copy(b[bp:], layer)
			} else {
				data := label.FormatMountLabel(fmt.Sprintf("append%s", layer), mountLabel)
				if err = mount("none", target, "aufs", syscall.MS_REMOUNT, data); err != nil {
					return
				}
			}
		}

		if firstMount {
			opts := "dio,xino=/dev/shm/aufs.xino"
			if useDirperm() {
				opts += ",dirperm1"
			}
			data := label.FormatMountLabel(fmt.Sprintf("%s,%s", string(b[:bp]), opts), mountLabel)
			if err = mount("none", target, "aufs", 0, data); err != nil {
				return
			}
			firstMount = false
		}

		if i == len(ro) {
			break
		}
	}

	return
}

// useDirperm checks dirperm1 mount option can be used with the current
// version of aufs.
func useDirperm() bool {
	enableDirpermLock.Do(func() {
		base, err := ioutil.TempDir("", "docker-aufs-base")
		if err != nil {
			logrus.Errorf("error checking dirperm1: %v", err)
			return
		}
		defer os.RemoveAll(base)

		union, err := ioutil.TempDir("", "docker-aufs-union")
		if err != nil {
			logrus.Errorf("error checking dirperm1: %v", err)
			return
		}
		defer os.RemoveAll(union)

		opts := fmt.Sprintf("br:%s,dirperm1,xino=/dev/shm/aufs.xino", base)
		if err := mount("none", union, "aufs", 0, opts); err != nil {
			return
		}
		enableDirperm = true
		if err := Unmount(union); err != nil {
			logrus.Errorf("error checking dirperm1: failed to unmount %v", err)
		}
	})
	return enableDirperm
}
