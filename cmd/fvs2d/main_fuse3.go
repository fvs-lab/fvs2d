//go:build fuse3

package main

/*
#cgo CFLAGS: -DFUSE_USE_VERSION=35 -I${SRCDIR} -I${SRCDIR}/../../../libfuse/include
#cgo LDFLAGS: -l:libfuse3.so.3 -lpthread -ldl

#include <errno.h>
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

extern char*  fvs_read(off_t off, size_t req, size_t* out_len, int* err_out);
extern ssize_t fvs_write(off_t off, char* buf, size_t n, int* err_out);
extern off_t  fvs_size(void);

static void ll_init(void *userdata, struct fuse_conn_info *conn)
{
	(void)userdata;
	(void)conn;
}

static void ll_lookup(fuse_req_t req, fuse_ino_t parent, const char *name)
{
	if (parent != 1) {
		fuse_reply_err(req, ENOENT);
		return;
	}
	if (strcmp(name, "data") != 0) {
		fuse_reply_err(req, ENOENT);
		return;
	}

	struct fuse_entry_param e;
	memset(&e, 0, sizeof(e));
	e.ino = 2;
	e.attr_timeout = 1.0;
	e.entry_timeout = 1.0;
	e.attr.st_ino = 2;
	e.attr.st_mode = S_IFREG | 0644;
	e.attr.st_nlink = 1;
	e.attr.st_size = (off_t)fvs_size();
	fuse_reply_entry(req, &e);
}

static void ll_getattr(fuse_req_t req, fuse_ino_t ino, struct fuse_file_info *fi)
{
	(void)fi;
	struct stat st;
	memset(&st, 0, sizeof(st));

	if (ino == 1) {
		st.st_ino = 1;
		st.st_mode = S_IFDIR | 0755;
		st.st_nlink = 2;
		fuse_reply_attr(req, &st, 1.0);
		return;
	}
	if (ino == 2) {
		st.st_ino = 2;
		st.st_mode = S_IFREG | 0644;
		st.st_nlink = 1;
		st.st_size = (off_t)fvs_size();
		fuse_reply_attr(req, &st, 1.0);
		return;
	}
	fuse_reply_err(req, ENOENT);
}

static void ll_readdir(fuse_req_t req, fuse_ino_t ino, size_t size, off_t off, struct fuse_file_info *fi)
{
	(void)fi;
	if (ino != 1) {
		fuse_reply_err(req, ENOTDIR);
		return;
	}
	if (off != 0) {
		fuse_reply_buf(req, NULL, 0);
		return;
	}

	char *buf = (char*)calloc(1, size);
	if (!buf) {
		fuse_reply_err(req, ENOMEM);
		return;
	}

	size_t used = 0;
	struct stat st;
	memset(&st, 0, sizeof(st));

	st.st_ino = 1;
	used += fuse_add_direntry(req, buf + used, size - used, ".", &st, used);
	used += fuse_add_direntry(req, buf + used, size - used, "..", &st, used);

	memset(&st, 0, sizeof(st));
	st.st_ino = 2;
	st.st_mode = S_IFREG | 0644;
	used += fuse_add_direntry(req, buf + used, size - used, "data", &st, used);

	if (used > size) used = size;
	fuse_reply_buf(req, buf, used);
	free(buf);
}

static void ll_open(fuse_req_t req, fuse_ino_t ino, struct fuse_file_info *fi)
{
	if (ino != 2) {
		fuse_reply_err(req, ENOENT);
		return;
	}
	fi->direct_io = 1;
	fuse_reply_open(req, fi);
}

static void ll_read(fuse_req_t req, fuse_ino_t ino, size_t size, off_t off, struct fuse_file_info *fi)
{
	(void)fi;
	if (ino != 2) {
		fuse_reply_err(req, ENOENT);
		return;
	}
	int err = 0;
	size_t out_len = 0;
	char* out = fvs_read(off, size, &out_len, &err);
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

static void ll_write(fuse_req_t req, fuse_ino_t ino, const char *buf, size_t size, off_t off, struct fuse_file_info *fi)
{
	(void)fi;
	if (ino != 2) {
		fuse_reply_err(req, ENOENT);
		return;
	}
	int err = 0;
	ssize_t n = fvs_write(off, (char*)buf, size, &err);
	if (err != 0) {
		fuse_reply_err(req, err);
		return;
	}
	if (n < 0) {
		fuse_reply_err(req, EIO);
		return;
	}
	fuse_reply_write(req, (size_t)n);
}

static struct fuse_lowlevel_ops g_ops = {
	.init = ll_init,
	.lookup = ll_lookup,
	.getattr = ll_getattr,
	.readdir = ll_readdir,
	.open = ll_open,
	.read = ll_read,
	.write = ll_write,
};

static struct fuse_lowlevel_ops* fvs_ops(void) { return &g_ops; }

*/
import "C"

import (
	"errors"
	"flag"
	"fmt"
	"io"
	"os"
	"os/signal"
	"path/filepath"
	"sync"
	"syscall"
	"unsafe"

	core "fvs-v2-core"
	"fvs2d/internal/runtime"
)

var (
	gMu   sync.Mutex
	gFile *core.CoWFile
)

func initState(blocksDir string, blockSize int) error {
	if blocksDir == "" {
		blocksDir = filepath.Join(".fvs2", "blocks")
	}
	bs, err := core.NewDiskBlockStore(blocksDir)
	if err != nil {
		return err
	}
	m := core.NewMemCoWMap()
	f, err := core.NewCoWFile(bs, m, blockSize)
	if err != nil {
		return err
	}
	gMu.Lock()
	gFile = f
	gMu.Unlock()
	return nil
}

//export fvs_size
func fvs_size() C.off_t {
	gMu.Lock()
	defer gMu.Unlock()
	if gFile == nil {
		return 0
	}
	return C.off_t(gFile.Size())
}

//export fvs_read
func fvs_read(off C.off_t, req C.size_t, outLen *C.size_t, errOut *C.int) *C.char {
	*errOut = 0
	*outLen = 0

	gMu.Lock()
	defer gMu.Unlock()
	f := gFile
	if f == nil {
		*errOut = C.int(syscall.EIO)
		return nil
	}

	buf := make([]byte, int(req))
	n, err := f.ReadAt(buf, int64(off))
	if err != nil {
		if errors.Is(err, io.EOF) {
			// allow short read
		} else if errors.Is(err, syscall.EINVAL) {
			*errOut = C.int(syscall.EINVAL)
			return nil
		} else {
			*errOut = C.int(syscall.EIO)
			return nil
		}
	}
	if n <= 0 {
		return nil
	}

	p := C.malloc(C.size_t(n))
	if p == nil {
		*errOut = C.int(syscall.ENOMEM)
		return nil
	}
	C.memcpy(p, unsafe.Pointer(&buf[0]), C.size_t(n))
	*outLen = C.size_t(n)
	return (*C.char)(p)
}

//export fvs_write
func fvs_write(off C.off_t, buf *C.char, n C.size_t, errOut *C.int) C.ssize_t {
	*errOut = 0

	gMu.Lock()
	defer gMu.Unlock()
	f := gFile
	if f == nil {
		*errOut = C.int(syscall.EIO)
		return -1
	}

	data := C.GoBytes(unsafe.Pointer(buf), C.int(n))
	w, err := f.WriteAt(data, int64(off))
	if err != nil {
		*errOut = C.int(syscall.EIO)
		return -1
	}
	return C.ssize_t(w)
}

func main() {
	var mountPoint string
	var blocksDir string
	var debug bool
	var blockSize int
	var probe bool

	flag.StringVar(&mountPoint, "mount", "", "mountpoint")
	flag.StringVar(&blocksDir, "blocks", "", "blocks dir (default: ./.fvs2/blocks)")
	flag.BoolVar(&debug, "debug", false, "enable debug")
	flag.IntVar(&blockSize, "block-size", 4096, "CoW block size")
	flag.BoolVar(&probe, "probe", false, "print capability report and exit")
	flag.Parse()

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
	if err := initState(blocksDir, blockSize); err != nil {
		fmt.Fprintf(os.Stderr, "ERR: init state: %v\n", err)
		os.Exit(1)
	}

	argv := []*C.char{C.CString("fvs2d"), C.CString("-f")}
	if debug {
		argv = append(argv, C.CString("-d"))
	}
	argv = append(argv, C.CString(mountPoint))
	defer func() {
		for _, a := range argv {
			C.free(unsafe.Pointer(a))
		}
	}()

	args := C.struct_fuse_args{argc: C.int(len(argv)), argv: &argv[0], allocated: 0}

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
		_ = <-errCh
		return
	case err := <-errCh:
		if err != nil {
			fmt.Fprintf(os.Stderr, "ERR: %v\n", err)
			os.Exit(1)
		}
	}
}
