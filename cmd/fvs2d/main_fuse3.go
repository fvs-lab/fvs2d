//go:build fuse3

package main

/*
#cgo pkg-config: fuse3
#cgo CFLAGS: -DFUSE_USE_VERSION=35 -I${SRCDIR}
#cgo LDFLAGS: -lpthread -ldl

#include <errno.h>
#include <fcntl.h>
#include <stdint.h>
#include <stdlib.h>
#include <string.h>
#include <sys/stat.h>
#include <time.h>
#include <unistd.h>

#include "fuse_lowlevel.h"

static time_t g_mtime;

// The system libfuse3 in this environment does not export fuse_session_new_versioned().
// Force the non-versioned entrypoint (fuse_session_new) and wrap it for cgo.
#ifdef fuse_session_new
#undef fuse_session_new
#endif
struct fuse_session *fuse_session_new(struct fuse_args *args,
				      const struct fuse_lowlevel_ops *op,
				      size_t op_size,
				      void *userdata);
static struct fuse_session *fvs_fuse_session_new(struct fuse_args *args,
						const struct fuse_lowlevel_ops *op,
						size_t op_size,
						void *userdata)
{
	return fuse_session_new(args, op, op_size, userdata);
}

// Tree callbacks implemented in Go (see exports below).
extern int      fvs_getattr(uint64_t ino, uint32_t* mode, int64_t* size, uint32_t* nlink);
extern uint64_t fvs_lookup(uint64_t parent, char* name);
extern long     fvs_readdir_count(uint64_t ino);
extern char*    fvs_readdir_at(uint64_t ino, long idx, uint64_t* out_ino, uint32_t* out_mode);
extern char*    fvs_read_node(uint64_t ino, off_t off, size_t req, size_t* out_len, int* err_out);
extern char*    fvs_readlink(uint64_t ino);
extern int      fvs_writable(void);
extern int      fvs_create(uint64_t parent, char* name, uint32_t mode, uint64_t* out_ino);
extern long     fvs_write(uint64_t ino, char* buf, size_t size, off_t off, int* err_out);
extern int      fvs_mkdir(uint64_t parent, char* name, uint32_t mode, uint64_t* out_ino);
extern int      fvs_remove(uint64_t parent, char* name);
extern int      fvs_truncate(uint64_t ino, int64_t size);
extern int      fvs_rename(uint64_t parent, char* name, uint64_t newparent, char* newname);

static void ll_init(void *userdata, struct fuse_conn_info *conn)
{
	(void)userdata;
	(void)conn;
	g_mtime = time(NULL);
}

static void ll_lookup(fuse_req_t req, fuse_ino_t parent, const char *name)
{
	uint64_t ino = fvs_lookup((uint64_t)parent, (char*)name);
	if (ino == 0) {
		fuse_reply_err(req, ENOENT);
		return;
	}
	uint32_t mode = 0, nlink = 1;
	int64_t size = 0;
	if (fvs_getattr(ino, &mode, &size, &nlink) != 0) {
		fuse_reply_err(req, ENOENT);
		return;
	}
	struct fuse_entry_param e;
	memset(&e, 0, sizeof(e));
	e.ino = ino;
	e.attr_timeout = 1.0;
	e.entry_timeout = 1.0;
	e.attr.st_ino = ino;
	e.attr.st_mode = mode;
	e.attr.st_nlink = nlink;
	e.attr.st_uid = getuid();
	e.attr.st_gid = getgid();
	e.attr.st_atime = e.attr.st_mtime = e.attr.st_ctime = g_mtime;
	e.attr.st_size = (off_t)size;
	fuse_reply_entry(req, &e);
}

static void ll_getattr(fuse_req_t req, fuse_ino_t ino, struct fuse_file_info *fi)
{
	(void)fi;
	uint32_t mode = 0, nlink = 1;
	int64_t size = 0;
	if (fvs_getattr((uint64_t)ino, &mode, &size, &nlink) != 0) {
		fuse_reply_err(req, ENOENT);
		return;
	}
	struct stat st;
	memset(&st, 0, sizeof(st));
	st.st_ino = ino;
	st.st_mode = mode;
	st.st_nlink = nlink;
	st.st_uid = getuid();
	st.st_gid = getgid();
	st.st_atime = st.st_mtime = st.st_ctime = g_mtime;
	st.st_size = (off_t)size;
	fuse_reply_attr(req, &st, 1.0);
}

static void ll_readdir(fuse_req_t req, fuse_ino_t ino, size_t size, off_t off, struct fuse_file_info *fi)
{
	(void)fi;
	long n = fvs_readdir_count((uint64_t)ino);
	if (n < 0) {
		fuse_reply_err(req, ENOTDIR);
		return;
	}
	char *buf = (char*)calloc(1, size);
	if (!buf) {
		fuse_reply_err(req, ENOMEM);
		return;
	}
	size_t used = 0;
	for (off_t i = off; ; i++) {
		struct stat st;
		memset(&st, 0, sizeof(st));
		const char *nm = NULL;
		char *freeme = NULL;
		if (i == 0) {
			nm = ".";
			st.st_ino = ino;
			st.st_mode = S_IFDIR | 0755;
		} else if (i == 1) {
			nm = "..";
			st.st_ino = 1;
			st.st_mode = S_IFDIR | 0755;
		} else {
			long ci = (long)(i - 2);
			if (ci >= n) break;
			uint64_t cino = 0;
			uint32_t cmode = 0;
			char *cname = fvs_readdir_at((uint64_t)ino, ci, &cino, &cmode);
			if (!cname) break;
			nm = cname;
			freeme = cname;
			st.st_ino = cino;
			st.st_mode = cmode;
		}
		size_t need = fuse_add_direntry(req, NULL, 0, nm, NULL, 0);
		if (used + need > size) {
			if (freeme) free(freeme);
			break;
		}
		used += fuse_add_direntry(req, buf + used, size - used, nm, &st, i + 1);
		if (freeme) free(freeme);
	}
	fuse_reply_buf(req, buf, used);
	free(buf);
}

static void ll_open(fuse_req_t req, fuse_ino_t ino, struct fuse_file_info *fi)
{
	int wr = (fi->flags & O_ACCMODE) != O_RDONLY;
	if (wr && !fvs_writable()) {
		fuse_reply_err(req, EROFS);
		return;
	}
	uint32_t mode = 0, nlink = 1;
	int64_t size = 0;
	if (fvs_getattr((uint64_t)ino, &mode, &size, &nlink) != 0) {
		fuse_reply_err(req, ENOENT);
		return;
	}
	if (wr && (fi->flags & O_TRUNC)) {
		fvs_truncate((uint64_t)ino, 0);
	}
	fuse_reply_open(req, fi);
}

static void ll_read(fuse_req_t req, fuse_ino_t ino, size_t size, off_t off, struct fuse_file_info *fi)
{
	(void)fi;
	int err = 0;
	size_t out_len = 0;
	char *out = fvs_read_node((uint64_t)ino, off, size, &out_len, &err);
	if (err != 0) {
		if (out) free(out);
		fuse_reply_err(req, err);
		return;
	}
	if (!out) {
		fuse_reply_buf(req, NULL, 0);
		return;
	}
	fuse_reply_buf(req, out, out_len);
	free(out);
}

static void ll_readlink(fuse_req_t req, fuse_ino_t ino)
{
	char *tgt = fvs_readlink((uint64_t)ino);
	if (!tgt) {
		fuse_reply_err(req, EINVAL);
		return;
	}
	fuse_reply_readlink(req, tgt);
	free(tgt);
}

static void ll_write(fuse_req_t req, fuse_ino_t ino, const char *buf, size_t size, off_t off, struct fuse_file_info *fi)
{
	(void)fi;
	if (!fvs_writable()) {
		fuse_reply_err(req, EROFS);
		return;
	}
	int err = 0;
	long n = fvs_write((uint64_t)ino, (char*)buf, size, off, &err);
	if (n < 0) {
		fuse_reply_err(req, err ? err : EIO);
		return;
	}
	fuse_reply_write(req, (size_t)n);
}

static void reply_new_entry(fuse_req_t req, uint64_t ino)
{
	uint32_t mode = 0, nlink = 1;
	int64_t size = 0;
	fvs_getattr(ino, &mode, &size, &nlink);
	struct fuse_entry_param e;
	memset(&e, 0, sizeof(e));
	e.ino = ino;
	e.attr_timeout = 1.0;
	e.entry_timeout = 1.0;
	e.attr.st_ino = ino;
	e.attr.st_mode = mode;
	e.attr.st_nlink = nlink;
	e.attr.st_uid = getuid();
	e.attr.st_gid = getgid();
	e.attr.st_atime = e.attr.st_mtime = e.attr.st_ctime = g_mtime;
	e.attr.st_size = (off_t)size;
	fuse_reply_entry(req, &e);
}

static void ll_create(fuse_req_t req, fuse_ino_t parent, const char *name, mode_t mode, struct fuse_file_info *fi)
{
	if (!fvs_writable()) {
		fuse_reply_err(req, EROFS);
		return;
	}
	uint64_t ino = 0;
	if (fvs_create((uint64_t)parent, (char*)name, (uint32_t)mode, &ino) != 0 || ino == 0) {
		fuse_reply_err(req, EIO);
		return;
	}
	uint32_t m = 0, nlink = 1;
	int64_t size = 0;
	fvs_getattr(ino, &m, &size, &nlink);
	struct fuse_entry_param e;
	memset(&e, 0, sizeof(e));
	e.ino = ino;
	e.attr_timeout = 1.0;
	e.entry_timeout = 1.0;
	e.attr.st_ino = ino;
	e.attr.st_mode = m;
	e.attr.st_nlink = nlink;
	e.attr.st_uid = getuid();
	e.attr.st_gid = getgid();
	e.attr.st_atime = e.attr.st_mtime = e.attr.st_ctime = g_mtime;
	e.attr.st_size = (off_t)size;
	fuse_reply_create(req, &e, fi);
}

static void ll_mkdir(fuse_req_t req, fuse_ino_t parent, const char *name, mode_t mode)
{
	if (!fvs_writable()) {
		fuse_reply_err(req, EROFS);
		return;
	}
	uint64_t ino = 0;
	if (fvs_mkdir((uint64_t)parent, (char*)name, (uint32_t)mode, &ino) != 0 || ino == 0) {
		fuse_reply_err(req, EIO);
		return;
	}
	reply_new_entry(req, ino);
}

static void ll_unlink(fuse_req_t req, fuse_ino_t parent, const char *name)
{
	if (!fvs_writable()) {
		fuse_reply_err(req, EROFS);
		return;
	}
	fuse_reply_err(req, fvs_remove((uint64_t)parent, (char*)name));
}

static void ll_setattr(fuse_req_t req, fuse_ino_t ino, struct stat *attr, int to_set, struct fuse_file_info *fi)
{
	(void)fi;
	if (to_set & FUSE_SET_ATTR_SIZE) {
		if (!fvs_writable()) {
			fuse_reply_err(req, EROFS);
			return;
		}
		if (fvs_truncate((uint64_t)ino, (int64_t)attr->st_size) != 0) {
			fuse_reply_err(req, EIO);
			return;
		}
	}
	uint32_t mode = 0, nlink = 1;
	int64_t size = 0;
	if (fvs_getattr((uint64_t)ino, &mode, &size, &nlink) != 0) {
		fuse_reply_err(req, ENOENT);
		return;
	}
	struct stat st;
	memset(&st, 0, sizeof(st));
	st.st_ino = ino;
	st.st_mode = mode;
	st.st_nlink = nlink;
	st.st_uid = getuid();
	st.st_gid = getgid();
	st.st_atime = st.st_mtime = st.st_ctime = g_mtime;
	st.st_size = (off_t)size;
	fuse_reply_attr(req, &st, 1.0);
}

static void ll_rename(fuse_req_t req, fuse_ino_t parent, const char *name,
		      fuse_ino_t newparent, const char *newname, unsigned int flags)
{
	(void)flags;
	if (!fvs_writable()) {
		fuse_reply_err(req, EROFS);
		return;
	}
	fuse_reply_err(req, fvs_rename((uint64_t)parent, (char*)name, (uint64_t)newparent, (char*)newname));
}

static void ll_fsync(fuse_req_t req, fuse_ino_t ino, int datasync, struct fuse_file_info *fi)
{
	(void)ino; (void)datasync; (void)fi;
	fuse_reply_err(req, 0);
}

static void ll_flush(fuse_req_t req, fuse_ino_t ino, struct fuse_file_info *fi)
{
	(void)ino; (void)fi;
	fuse_reply_err(req, 0);
}

static struct fuse_lowlevel_ops g_ops = {
	.init = ll_init,
	.lookup = ll_lookup,
	.getattr = ll_getattr,
	.setattr = ll_setattr,
	.readdir = ll_readdir,
	.readlink = ll_readlink,
	.open = ll_open,
	.read = ll_read,
	.write = ll_write,
	.create = ll_create,
	.mkdir = ll_mkdir,
	.unlink = ll_unlink,
	.rmdir = ll_unlink,
	.rename = ll_rename,
	.fsync = ll_fsync,
	.flush = ll_flush,
};

static struct fuse_lowlevel_ops* fvs_ops(void) { return &g_ops; }

*/
import "C"

import (
	"flag"
	"fmt"
	"os"
	"os/exec"
	"os/signal"
	"strings"
	"sync"
	"syscall"
	"time"
	"unsafe"

	"google.golang.org/grpc"

	pb "fvs2d/internal/controlpb"
	"fvs2d/internal/runtime"
)

// unmount detaches the FUSE mount so the blocked session loop can return.
//
// fuse_session_exit only sets a flag; a loop parked on a read of /dev/fuse
// keeps waiting until woken. A signal wakes it via EINTR, a gRPC Shutdown does
// not, so we unmount to break the loop. lazy detaches even if the mount is
// busy. Errors are ignored: the deferred fuse_session_unmount is the fallback.
func unmount(mp string, lazy bool) {
	arg := "-u"
	if lazy {
		arg = "-uz"
	}
	_ = exec.Command("fusermount3", arg, mp).Run()
}

var (
	gLock sync.Mutex
	gOv   *overlay
)

//export fvs_writable
func fvs_writable() C.int {
	gLock.Lock()
	defer gLock.Unlock()
	if gOv != nil && gOv.writable() {
		return 1
	}
	return 0
}

//export fvs_getattr
func fvs_getattr(ino C.uint64_t, mode *C.uint32_t, size *C.int64_t, nlink *C.uint32_t) C.int {
	gLock.Lock()
	defer gLock.Unlock()
	if gOv == nil {
		return -1
	}
	m, sz, nl, ok := gOv.getattr(uint64(ino))
	if !ok {
		return -1
	}
	*mode = C.uint32_t(m)
	*size = C.int64_t(sz)
	*nlink = C.uint32_t(nl)
	return 0
}

//export fvs_lookup
func fvs_lookup(parent C.uint64_t, name *C.char) C.uint64_t {
	gLock.Lock()
	defer gLock.Unlock()
	if gOv == nil {
		return 0
	}
	return C.uint64_t(gOv.lookup(uint64(parent), C.GoString(name)))
}

var gDir = map[uint64][]dirent{}

//export fvs_readdir_count
func fvs_readdir_count(ino C.uint64_t) C.long {
	gLock.Lock()
	defer gLock.Unlock()
	if gOv == nil {
		return -1
	}
	ents, ok := gOv.listDir(uint64(ino))
	if !ok {
		return -1
	}
	gDir[uint64(ino)] = ents
	return C.long(len(ents))
}

//export fvs_readdir_at
func fvs_readdir_at(ino C.uint64_t, idx C.long, outIno *C.uint64_t, outMode *C.uint32_t) *C.char {
	gLock.Lock()
	defer gLock.Unlock()
	ents := gDir[uint64(ino)]
	i := int(idx)
	if i < 0 || i >= len(ents) {
		return nil
	}
	e := ents[i]
	*outIno = C.uint64_t(e.ino)
	*outMode = C.uint32_t(e.mode)
	return C.CString(e.name)
}

//export fvs_read_node
func fvs_read_node(ino C.uint64_t, off C.off_t, req C.size_t, outLen *C.size_t, errOut *C.int) *C.char {
	*errOut = 0
	*outLen = 0
	gLock.Lock()
	defer gLock.Unlock()
	if gOv == nil {
		*errOut = C.int(syscall.EIO)
		return nil
	}
	data, errno := gOv.readAt(uint64(ino), int64(off), int(req))
	if errno != 0 {
		*errOut = C.int(errno)
		return nil
	}
	if len(data) == 0 {
		return nil
	}
	p := C.malloc(C.size_t(len(data)))
	if p == nil {
		*errOut = C.int(syscall.ENOMEM)
		return nil
	}
	C.memcpy(p, unsafe.Pointer(&data[0]), C.size_t(len(data)))
	*outLen = C.size_t(len(data))
	return (*C.char)(p)
}

//export fvs_readlink
func fvs_readlink(ino C.uint64_t) *C.char {
	gLock.Lock()
	defer gLock.Unlock()
	if gOv == nil {
		return nil
	}
	tgt, ok := gOv.readlink(uint64(ino))
	if !ok {
		return nil
	}
	return C.CString(tgt)
}

//export fvs_create
func fvs_create(parent C.uint64_t, name *C.char, mode C.uint32_t, outIno *C.uint64_t) C.int {
	gLock.Lock()
	defer gLock.Unlock()
	if gOv == nil {
		return -1
	}
	ino, errno := gOv.create(uint64(parent), C.GoString(name), uint32(mode))
	if errno != 0 {
		return -1
	}
	*outIno = C.uint64_t(ino)
	return 0
}

//export fvs_write
func fvs_write(ino C.uint64_t, buf *C.char, size C.size_t, off C.off_t, errOut *C.int) C.long {
	*errOut = 0
	gLock.Lock()
	defer gLock.Unlock()
	if gOv == nil {
		*errOut = C.int(syscall.EIO)
		return -1
	}
	data := C.GoBytes(unsafe.Pointer(buf), C.int(size))
	n, errno := gOv.write(uint64(ino), data, int64(off))
	if errno != 0 {
		*errOut = C.int(errno)
		return -1
	}
	return C.long(n)
}

//export fvs_mkdir
func fvs_mkdir(parent C.uint64_t, name *C.char, mode C.uint32_t, outIno *C.uint64_t) C.int {
	gLock.Lock()
	defer gLock.Unlock()
	if gOv == nil {
		return -1
	}
	ino, errno := gOv.mkdir(uint64(parent), C.GoString(name), uint32(mode))
	if errno != 0 {
		return -1
	}
	*outIno = C.uint64_t(ino)
	return 0
}

//export fvs_remove
func fvs_remove(parent C.uint64_t, name *C.char) C.int {
	gLock.Lock()
	defer gLock.Unlock()
	if gOv == nil {
		return C.int(syscall.EIO)
	}
	return C.int(gOv.remove(uint64(parent), C.GoString(name)))
}

//export fvs_truncate
func fvs_truncate(ino C.uint64_t, size C.int64_t) C.int {
	gLock.Lock()
	defer gLock.Unlock()
	if gOv == nil {
		return -1
	}
	if gOv.truncate(uint64(ino), int64(size)) != 0 {
		return -1
	}
	return 0
}

//export fvs_rename
func fvs_rename(parent C.uint64_t, name *C.char, newparent C.uint64_t, newname *C.char) C.int {
	gLock.Lock()
	defer gLock.Unlock()
	if gOv == nil {
		return C.int(syscall.EIO)
	}
	return C.int(gOv.rename(uint64(parent), C.GoString(name), uint64(newparent), C.GoString(newname)))
}

type stringList []string

func (s *stringList) String() string { return strings.Join(*s, ",") }
func (s *stringList) Set(v string) error {
	*s = append(*s, v)
	return nil
}

func main() {
	var mountPoint string
	var blocksDir string
	var repoDir string
	var stateSel string
	var branchSel string
	var lowers stringList
	var upperDir string
	var debug bool
	var blockSize int
	var probe bool
	var controlAddr string

	flag.StringVar(&mountPoint, "mount", "", "mountpoint")
	flag.StringVar(&repoDir, "repo", ".", "repo root containing .fvs2")
	flag.StringVar(&stateSel, "state", "", "state id/prefix to mount (default: HEAD)")
	flag.StringVar(&branchSel, "branch", "", "branch to mount (default: HEAD)")
	flag.Var(&lowers, "lower", "lower layer repo (repeatable, low-to-high): repo | repo@state | repo#branch")
	flag.StringVar(&upperDir, "upper", "", "writable upper layer dir (enables writes; omit for read-only)")
	flag.StringVar(&blocksDir, "blocks", "", "blocks dir override (default: <repo>/.fvs2/blocks)")
	flag.BoolVar(&debug, "debug", false, "enable debug")
	flag.IntVar(&blockSize, "block-size", 4096, "fallback block size (overridden by the commit)")
	flag.BoolVar(&probe, "probe", false, "print capability report and exit")
	flag.StringVar(&controlAddr, "control", "", "enable gRPC control server: unix:/path.sock or tcp:host:port (empty = disabled)")
	flag.Parse()
	_ = blockSize // block size is taken from the committed state

	flatpak := runtime.DetectFlatpak()
	devFuse := runtime.DevFuseAccessible()
	fmount := runtime.FusermountAvailable()

	if probe {
		fmt.Printf("flatpak=%v\n", flatpak)
		fmt.Printf("dev_fuse_accessible=%v\n", devFuse)
		fmt.Printf("fusermount_available=%v\n", fmount)
		return
	}
	if mountPoint == "" {
		fmt.Fprintln(os.Stderr, "ERR: --mount is required")
		os.Exit(2)
	}
	if !devFuse || !fmount {
		fmt.Fprintf(os.Stderr, "ERR: cannot mount (dev_fuse_accessible=%v fusermount_available=%v)\n", devFuse, fmount)
		os.Exit(1)
	}

	var tree *fsTree
	var err error
	if len(lowers) > 0 {
		sels := make([]layerSel, 0, len(lowers))
		for _, s := range lowers {
			sels = append(sels, parseLayerSel(s))
		}
		tree, err = buildMergedTreeFromRepos(sels, blocksDir)
	} else {
		tree, err = buildTreeFromRepo(repoDir, stateSel, branchSel, blocksDir)
	}
	if err != nil {
		fmt.Fprintf(os.Stderr, "ERR: %v\n", err)
		os.Exit(1)
	}
	if upperDir != "" {
		if err := os.MkdirAll(upperDir, 0o755); err != nil {
			fmt.Fprintf(os.Stderr, "ERR: upper dir: %v\n", err)
			os.Exit(1)
		}
	}
	gLock.Lock()
	gOv = newOverlay(tree, upperDir)
	gLock.Unlock()

	// fuse_session_new() only accepts low-level options (e.g. -d). The
	// mountpoint is passed separately to fuse_session_mount() below, and -f
	// (foreground) is a high-level cmdline flag it does not understand: passing
	// either here makes fuse_opt_parse fail with "unknown option". We run the
	// session loop in this process, so foreground is implicit.
	argv := []*C.char{C.CString("fvs2d")}
	if debug {
		argv = append(argv, C.CString("-d"))
	}
	defer func() {
		for _, a := range argv {
			C.free(unsafe.Pointer(a))
		}
	}()

	// The fuse_args.argv field must point at C memory. Building it from a Go
	// slice (&argv[0]) hands C a Go pointer to memory holding other pointers,
	// which the cgo pointer checker rejects at runtime (panic). Copy the vector
	// into a C-allocated array so nothing Go-managed crosses the boundary.
	cArgvSize := C.size_t(len(argv)) * C.size_t(unsafe.Sizeof(uintptr(0)))
	cArgv := (**C.char)(C.malloc(cArgvSize))
	defer C.free(unsafe.Pointer(cArgv))
	cArgvSlice := unsafe.Slice(cArgv, len(argv))
	for i, a := range argv {
		cArgvSlice[i] = a
	}

	args := C.struct_fuse_args{argc: C.int(len(argv)), argv: cArgv, allocated: 0}

	se := C.fvs_fuse_session_new(&args, C.fvs_ops(), C.size_t(C.sizeof_struct_fuse_lowlevel_ops), nil)
	if se == nil {
		fmt.Fprintln(os.Stderr, "ERR: fuse_session_new failed")
		os.Exit(1)
	}
	defer C.fuse_session_destroy(se)

	mp := C.CString(mountPoint)
	defer C.free(unsafe.Pointer(mp))
	if C.fuse_session_mount(se, mp) != 0 {
		fmt.Fprintln(os.Stderr, "ERR: fuse_session_mount failed")
		os.Exit(1)
	}
	defer C.fuse_session_unmount(se)

	// Optional gRPC control plane. Shutdown is funneled through a channel so the
	// session loop below stays the only place that touches the C session handle.
	shutdownReq := make(chan bool, 1)
	startTime := time.Now()

	statusFn := func() *pb.GetStatusResponse {
		gLock.Lock()
		writable := gOv != nil && gOv.writable()
		upper := ""
		if gOv != nil {
			upper = gOv.upper
		}
		gLock.Unlock()

		resp := &pb.GetStatusResponse{
			Mountpoint:    mountPoint,
			Writable:      writable,
			Upper:         upper,
			NodeCount:     uint32(len(tree.nodes)),
			BlockSize:     int32(tree.blockSize),
			Debug:         debug,
			Pid:           int32(os.Getpid()),
			UptimeSeconds: int64(time.Since(startTime).Seconds()),
			ApiVersion:    controlAPIVersion,
		}
		if len(lowers) > 0 {
			for _, s := range lowers {
				sel := parseLayerSel(s)
				resp.Layers = append(resp.Layers, &pb.Layer{
					Repo:   sel.repo,
					State:  sel.state,
					Branch: sel.branch,
				})
			}
		} else {
			resp.Repo = repoDir
			resp.State = stateSel
			resp.Branch = branchSel
		}
		return resp
	}

	var control *grpc.Server
	if controlAddr != "" {
		srv := &controlServer{
			statusFn: statusFn,
			shutdownFn: func(lazy bool) {
				select {
				case shutdownReq <- lazy:
				default: // a shutdown is already pending
				}
			},
		}
		var err error
		control, err = startControlServer(controlAddr, srv)
		if err != nil {
			fmt.Fprintf(os.Stderr, "ERR: %v\n", err)
			os.Exit(1)
		}
		fmt.Fprintf(os.Stderr, "control: serving on %s\n", controlAddr)
	}

	sigCh := make(chan os.Signal, 1)
	signal.Notify(sigCh, syscall.SIGINT, syscall.SIGTERM)

	errCh := make(chan error, 1)
	go func() {
		ret := C.fuse_session_loop(se)
		if ret != 0 {
			errCh <- fmt.Errorf("fuse_session_loop=%d", int(ret))
			return
		}
		errCh <- nil
	}()

	select {
	case <-sigCh:
		C.fuse_session_exit(se)
		if control != nil {
			control.Stop()
		}
		<-errCh
		return
	case lazy := <-shutdownReq:
		// Set the exit flag, then unmount to wake the parked session loop.
		C.fuse_session_exit(se)
		unmount(mountPoint, lazy)
		if control != nil {
			// GracefulStop lets the in-flight Shutdown RPC flush its reply.
			control.GracefulStop()
		}
		<-errCh
		return
	case err := <-errCh:
		if control != nil {
			control.Stop()
		}
		if err != nil {
			fmt.Fprintf(os.Stderr, "ERR: %v\n", err)
			os.Exit(1)
		}
	}
}
