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

#include "fuse_lowlevel.h"

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

static void ll_init(void *userdata, struct fuse_conn_info *conn)
{
	(void)userdata;
	(void)conn;
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
	if ((fi->flags & O_ACCMODE) != O_RDONLY) {
		fuse_reply_err(req, EROFS);
		return;
	}
	uint32_t mode = 0, nlink = 1;
	int64_t size = 0;
	if (fvs_getattr((uint64_t)ino, &mode, &size, &nlink) != 0) {
		fuse_reply_err(req, ENOENT);
		return;
	}
	fi->direct_io = 1;
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
	(void)ino; (void)buf; (void)size; (void)off; (void)fi;
	// The mounted view is a read-only snapshot of a committed state.
	fuse_reply_err(req, EROFS);
}

static struct fuse_lowlevel_ops g_ops = {
	.init = ll_init,
	.lookup = ll_lookup,
	.getattr = ll_getattr,
	.readdir = ll_readdir,
	.readlink = ll_readlink,
	.open = ll_open,
	.read = ll_read,
	.write = ll_write,
};

static struct fuse_lowlevel_ops* fvs_ops(void) { return &g_ops; }

*/
import "C"

import (
	"flag"
	"fmt"
	"os"
	"os/signal"
	"sync"
	"syscall"
	"unsafe"

	"fvs2d/internal/runtime"
)

var (
	gMu   sync.RWMutex
	gTree *fsTree
)

// nodeStMode encodes a node's full st_mode (type bits + perms) for FUSE.
func nodeStMode(n *fileNode) uint32 {
	switch {
	case n.isDir:
		return syscall.S_IFDIR | (n.mode & 0o7777)
	case n.link != "":
		return syscall.S_IFLNK | 0o777
	default:
		return syscall.S_IFREG | (n.mode & 0o7777)
	}
}

//export fvs_getattr
func fvs_getattr(ino C.uint64_t, mode *C.uint32_t, size *C.int64_t, nlink *C.uint32_t) C.int {
	gMu.RLock()
	defer gMu.RUnlock()
	if gTree == nil {
		return -1
	}
	n := gTree.get(uint64(ino))
	if n == nil {
		return -1
	}
	*mode = C.uint32_t(nodeStMode(n))
	*size = C.int64_t(n.size)
	if n.isDir {
		*nlink = 2
	} else {
		*nlink = 1
	}
	return 0
}

//export fvs_lookup
func fvs_lookup(parent C.uint64_t, name *C.char) C.uint64_t {
	gMu.RLock()
	defer gMu.RUnlock()
	if gTree == nil {
		return 0
	}
	return C.uint64_t(gTree.lookup(uint64(parent), C.GoString(name)))
}

//export fvs_readdir_count
func fvs_readdir_count(ino C.uint64_t) C.long {
	gMu.RLock()
	defer gMu.RUnlock()
	if gTree == nil {
		return -1
	}
	n := gTree.get(uint64(ino))
	if n == nil || !n.isDir {
		return -1
	}
	return C.long(len(n.childOrder))
}

//export fvs_readdir_at
func fvs_readdir_at(ino C.uint64_t, idx C.long, outIno *C.uint64_t, outMode *C.uint32_t) *C.char {
	gMu.RLock()
	defer gMu.RUnlock()
	if gTree == nil {
		return nil
	}
	d := gTree.get(uint64(ino))
	if d == nil || !d.isDir {
		return nil
	}
	i := int(idx)
	if i < 0 || i >= len(d.childOrder) {
		return nil
	}
	c := gTree.get(d.childOrder[i])
	if c == nil {
		return nil
	}
	*outIno = C.uint64_t(c.ino)
	*outMode = C.uint32_t(nodeStMode(c))
	return C.CString(c.name)
}

//export fvs_read_node
func fvs_read_node(ino C.uint64_t, off C.off_t, req C.size_t, outLen *C.size_t, errOut *C.int) *C.char {
	*errOut = 0
	*outLen = 0
	gMu.RLock()
	defer gMu.RUnlock()
	if gTree == nil {
		*errOut = C.int(syscall.EIO)
		return nil
	}
	n := gTree.get(uint64(ino))
	if n == nil {
		*errOut = C.int(syscall.ENOENT)
		return nil
	}
	if n.isDir {
		*errOut = C.int(syscall.EISDIR)
		return nil
	}
	data, err := gTree.readAt(n, int64(off), int(req))
	if err != nil {
		// Includes ErrBlockCorrupt: surface as an I/O error rather than serving bad bytes.
		*errOut = C.int(syscall.EIO)
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
	gMu.RLock()
	defer gMu.RUnlock()
	if gTree == nil {
		return nil
	}
	n := gTree.get(uint64(ino))
	if n == nil || n.link == "" {
		return nil
	}
	return C.CString(n.link)
}

func main() {
	var mountPoint string
	var blocksDir string
	var repoDir string
	var stateSel string
	var branchSel string
	var debug bool
	var blockSize int
	var probe bool

	flag.StringVar(&mountPoint, "mount", "", "mountpoint")
	flag.StringVar(&repoDir, "repo", ".", "repo root containing .fvs2")
	flag.StringVar(&stateSel, "state", "", "state id/prefix to mount (default: HEAD)")
	flag.StringVar(&branchSel, "branch", "", "branch to mount (default: HEAD)")
	flag.StringVar(&blocksDir, "blocks", "", "blocks dir override (default: <repo>/.fvs2/blocks)")
	flag.BoolVar(&debug, "debug", false, "enable debug")
	flag.IntVar(&blockSize, "block-size", 4096, "fallback block size (overridden by the commit)")
	flag.BoolVar(&probe, "probe", false, "print capability report and exit")
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

	tree, err := buildTreeFromRepo(repoDir, stateSel, branchSel, blocksDir)
	if err != nil {
		fmt.Fprintf(os.Stderr, "ERR: %v\n", err)
		os.Exit(1)
	}
	gMu.Lock()
	gTree = tree
	gMu.Unlock()

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
		<-errCh
		return
	case err := <-errCh:
		if err != nil {
			fmt.Fprintf(os.Stderr, "ERR: %v\n", err)
			os.Exit(1)
		}
	}
}
