#!/usr/bin/env python3
"""Exercise a real mount. The signed FSKit extension must already be enabled."""
import argparse
import concurrent.futures
import hashlib
import json
import mmap
import os
from pathlib import Path
import platform
import random
import signal
import statistics
import subprocess
import sys
import time


def run(command, **kwargs):
    return subprocess.run([str(value) for value in command], check=True, **kwargs)


def wait_mount(point, process, timeout=30):
    deadline = time.monotonic() + timeout
    while time.monotonic() < deadline:
        if point.is_mount():
            return
        if process.poll() is not None:
            raise RuntimeError(f"mount process exited with {process.returncode}; inspect daemon.log")
        time.sleep(0.05)
    raise TimeoutError("real mount did not appear within 30 seconds")


def rss(pid):
    output = subprocess.check_output(["ps", "-o", "rss=", "-p", str(pid)], text=True)
    return int(output.strip()) * 1024


def extension_memory():
    output = subprocess.check_output(["ps", "-axo", "pid=,rss=,command="], text=True)
    result = []
    for line in output.splitlines():
        fields = line.split(None, 2)
        if len(fields) == 3 and "/FVS.appex/Contents/MacOS/FVS" in fields[2]:
            result.append({"pid": int(fields[0]), "rss_bytes": int(fields[1]) * 1024})
    return result


def digest(path):
    result = hashlib.sha256()
    with path.open("rb") as file:
        for chunk in iter(lambda: file.read(1 << 20), b""):
            result.update(chunk)
    return result.hexdigest()


def check_tree(point, data, expected_hash):
    assert (point / "dir/empty").stat().st_size == 0
    assert os.readlink(point / "link") == "dir/large.bin"
    assert (point / "link").read_bytes() == data
    assert (point / "clash.txt").read_text() == "higher"
    assert (point / "Case").read_text() == "capital"
    assert (point / "case").read_text() == "lowercase"
    assert (point / "\u00e9").read_text() == "composed"
    assert (point / "e\u0301").read_text() == "decomposed"
    assert not (point / "gone.txt").exists()
    assert not (point / "opaque/old").exists()
    assert (point / "opaque/new").read_text() == "new"
    assert (point / "dir").stat().st_mode & 0o777 == 0o750
    assert digest(point / "dir/large.bin") == expected_hash


def random_reads(point, data):
    rng = random.Random(33)
    timings = []
    fd = os.open(point / "dir/large.bin", os.O_RDONLY)
    try:
        for _ in range(1000):
            offset = rng.randrange(len(data) + 4096)
            length = rng.randrange(1, 32768)
            start = time.perf_counter_ns()
            chunk = os.pread(fd, length, offset)
            timings.append(time.perf_counter_ns() - start)
            assert chunk == data[offset:offset + length]
        assert os.pread(fd, 10, len(data)) == b""
        with mmap.mmap(fd, 0, access=mmap.ACCESS_READ) as mapped:
            assert mapped[8191:16397] == data[8191:16397]
    finally:
        os.close(fd)
    return {"median_ns": statistics.median(timings), "p95_ns": sorted(timings)[949]}


def concurrent_reads(point, data):
    def reader(seed):
        rng = random.Random(seed)
        fd = os.open(point / "dir/large.bin", os.O_RDONLY)
        try:
            for _ in range(100):
                offset = rng.randrange(len(data) - 4096)
                assert os.pread(fd, 4096, offset) == data[offset:offset + 4096]
        finally:
            os.close(fd)
    with concurrent.futures.ThreadPoolExecutor(max_workers=8) as executor:
        list(executor.map(reader, range(8)))


def writable(point, data):
    with (point / "dir/large.bin").open("rb", buffering=0) as reader:
        with (point / "dir/large.bin").open("r+b", buffering=0) as writer:
            writer.seek(37)
            writer.write(b"UP")
            writer.flush()
            os.fsync(writer.fileno())
            reader.seek(37)
            assert reader.read(2) == b"UP"
            os.rename(point / "dir/large.bin", point / "moved.bin")
            writer.truncate(40)
            writer.truncate(50000)
            reader.seek(42)
            assert reader.read(4096) == bytes(4096)
            os.unlink(point / "moved.bin")
            writer.truncate(20)
            writer.seek(0)
            writer.write(b"open after unlink")
            reader.seek(0)
            assert reader.read(17) == b"open after unlink"
    assert not (point / "dir/large.bin").exists()
    directory = point / "created"
    directory.mkdir()
    (directory / "file").write_bytes(b"created")
    os.chmod(directory / "file", 0o640)
    os.rename(directory, point / "renamed")
    assert (point / "renamed/file").read_bytes() == b"created"
    (point / "renamed/file").unlink()
    (point / "renamed").rmdir()
    os.symlink("clash.txt", point / "newlink")
    assert (point / "newlink").read_text() == "higher"
    link_time = 1_600_000_000_000_000_000
    target_time = (point / "clash.txt").stat().st_mtime_ns
    os.utime(point / "newlink", ns=(link_time, link_time), follow_symlinks=False)
    assert (point / "newlink").lstat().st_mtime_ns == link_time
    assert (point / "clash.txt").stat().st_mtime_ns == target_time
    os.lchown(point / "newlink", os.getuid(), os.getgid())
    (point / "newlink").unlink()
    for size in range(1, 33):
        path = point / f"small-{size}"
        payload = bytes((index + size) % 251 for index in range(size))
        with path.open("w+b", buffering=0) as file:
            assert file.write(payload) == size
            os.fsync(file.fileno())
            file.seek(0)
            assert file.read() == payload
        assert path.read_bytes() == payload
        path.unlink()
    os.rename(point / "\u00e9", point / "renamed-unicode")
    (point / "e\u0301").write_text("changed decomposition")
    assert (point / "renamed-unicode").read_text() == "composed"
    assert (point / "e\u0301").read_text() == "changed decomposition"
    os.rename(point / "opaque", point / "renamed-opaque")
    assert (point / "renamed-opaque/new").read_text() == "new"
    (point / "opaque").mkdir()
    assert list((point / "opaque").iterdir()) == []


def main():
    parser = argparse.ArgumentParser(description=__doc__)
    parser.add_argument("--daemon", required=True, type=Path)
    parser.add_argument("--fvs2", required=True, type=Path)
    parser.add_argument("--work-dir", required=True, type=Path)
    parser.add_argument("--finder", action="store_true", help="require a human Finder verification")
    args = parser.parse_args()
    args.daemon = args.daemon.resolve()
    args.fvs2 = args.fvs2.resolve()
    work = args.work_dir.resolve()
    work.mkdir(parents=True, exist_ok=True)
    if any(work.iterdir()):
        parser.error("--work-dir must be empty")
    temporary = work / "temporary"
    temporary.mkdir()
    for name in ("TMPDIR", "TMP", "TEMP"):
        os.environ[name] = str(temporary)
    source = work / "source"
    base, higher, upper, point = (source / "base", source / "higher", source / "upper", work / "mount")
    for path in (base, higher, upper, point):
        path.mkdir(parents=True)
    data = hashlib.shake_256(b"FVS native mount fixture").digest(16 << 20)
    expected_hash = hashlib.sha256(data).hexdigest()
    (base / "dir").mkdir(mode=0o750)
    (base / "dir/large.bin").write_bytes(data)
    (base / "dir/empty").touch()
    os.symlink("dir/large.bin", base / "link")
    (base / "Case").write_text("capital")
    (higher / "case").write_text("lowercase")
    (base / "\u00e9").write_text("composed")
    (higher / "e\u0301").write_text("decomposed")
    (base / "gone.txt").write_text("hidden")
    (base / "clash.txt").write_text("lower")
    (base / "opaque").mkdir()
    (base / "opaque/old").write_text("hidden")
    (higher / ".wh.gone.txt").touch()
    (higher / "clash.txt").write_text("higher")
    (higher / "opaque").mkdir()
    (higher / "opaque/.wh..wh..opq").touch()
    (higher / "opaque/new").write_text("new")
    c_source = work / "fixture.c"
    c_source.write_text("int answer(void) { return 42; }\nint main(void) { return 0; }\n")
    cc = ["xcrun", "clang"] if platform.system() == "Darwin" else ["cc"]
    library = "answer.dylib" if platform.system() == "Darwin" else "answer.so"
    run(cc + [c_source, "-o", base / "executable"])
    run(cc + (["-dynamiclib"] if platform.system() == "Darwin" else ["-shared", "-fPIC"]) + [c_source, "-o", base / library])
    if platform.system() == "Darwin":
        for path in (base / "executable", base / library):
            run(["codesign", "--force", "--sign", "-", path])
    for repository in (base, higher):
        run([args.fvs2, f"--path={repository}", "init", "--format=4"])
        run([args.fvs2, f"--path={repository}", "commit", "-m", "mount fixture"])
    results = {"platform": platform.platform(), "sha256": expected_hash, "mounts": []}
    process = None
    try:
        for iteration in range(3):
            command = [args.daemon, "-mount", point, "-lower", base, "-lower", higher]
            if platform.system() == "Darwin":
                command += ["-source", source]
            if iteration == 2:
                command += ["-upper", upper]
            with (work / f"daemon-{iteration}.log").open("wb") as log:
                start = time.perf_counter_ns()
                process = subprocess.Popen([str(value) for value in command], stdout=log, stderr=log)
                wait_mount(point, process)
                mounted = time.perf_counter_ns()
                with (point / "dir/large.bin").open("rb", buffering=0) as file:
                    first = time.perf_counter_ns()
                    assert file.read(3) == data[:3]
                    first_read = time.perf_counter_ns() - first
                assert not [entry for entry in upper.iterdir() if not entry.name.startswith(".wh.")], "mount or first read materialized the snapshot"
                metrics = {"mount_ns": mounted - start, "first_3byte_read_ns": first_read,
                           "daemon_rss_bytes": rss(process.pid)}
                if platform.system() == "Darwin":
                    metrics["visible_fvs_extension_processes"] = extension_memory()
                check_tree(point, data, expected_hash)
                start = time.perf_counter_ns()
                assert digest(point / "dir/large.bin") == expected_hash
                metrics["sequential_mib_per_second"] = (len(data) / (1 << 20)) / ((time.perf_counter_ns() - start) / 1e9)
                metrics["random_reads"] = random_reads(point, data)
                concurrent_reads(point, data)
                run([point / "executable"])
                code = "import ctypes,sys; lib=ctypes.CDLL(sys.argv[1]); assert lib.answer()==42"
                run([sys.executable, "-c", code, point / library])
                if args.finder and iteration == 0:
                    run(["open", point])
                    if input("Verify nested folders, symlinks and file preview in Finder; type PASS: ") != "PASS":
                        raise RuntimeError("Finder was not verified")
                    results["finder"] = "human verified"
                if iteration == 2:
                    writable(point, data)
                    assert digest(base / "dir/large.bin") == expected_hash, "lower working tree changed"
                    process.kill()
                    process.wait(timeout=10)
                    if platform.system() == "Darwin":
                        assert (point / "clash.txt").read_text() == "higher", "FSKit depended on daemon lifetime"
                    run([args.daemon, "-unmount", point])
                else:
                    try:
                        with (point / "dir/large.bin").open("r+b"):
                            raise AssertionError("read-only snapshot allowed a writable open")
                    except OSError as error:
                        assert error.errno in (30, 13), error
                    process.send_signal(signal.SIGTERM)
                    assert process.wait(timeout=20) == 0
                assert not point.is_mount()
                metrics["checks"] = "hash, random, EOF, concurrent, mmap, executable, library, lifecycle passed"
                results["mounts"].append(metrics)
                process = None
        if not args.finder:
            results["finder"] = "NOT VERIFIED (run with --finder)"
        results["extension_crash"] = "NOT VERIFIED (requires a separately identified FSKit extension process)"
        (work / "results.json").write_text(json.dumps(results, indent=2) + "\n")
        print(json.dumps(results, indent=2))
    finally:
        if point.is_mount():
            subprocess.run([str(args.daemon), "-unmount", str(point)], check=False)
        if process is not None and process.poll() is None:
            process.terminate()
            try:
                process.wait(timeout=10)
            except subprocess.TimeoutExpired:
                process.kill()
                process.wait()


if __name__ == "__main__":
    main()
