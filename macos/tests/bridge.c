#include "libfvs.h"
#include <assert.h>
#include <errno.h>
#include <stdint.h>
#include <stdio.h>
#include <stdlib.h>
#include <string.h>
#include <time.h>

static uint64_t open_file(uint64_t session, int write) {
    char *reply = fvs_call(session, write ?
        "{\"op\":\"open\",\"path\":\"large.bin\",\"write\":true}" :
        "{\"op\":\"open\",\"path\":\"large.bin\"}");
    assert(reply && strstr(reply, "\"errno\":0"));
    char *handle = strstr(reply, "\"handle\":");
    assert(handle);
    uint64_t id = strtoull(handle + strlen("\"handle\":"), NULL, 10);
    free(reply);
    return id;
}

int main(int argc, char **argv) {
    assert(argc == 2);
    uint64_t session = 0;
    char *error = fvs_open(argv[1], &session);
    if (error) { fprintf(stderr, "%s\n", error); free(error); return 1; }
    char *invalid = fvs_call(session, "");
    assert(invalid && strstr(invalid, "\"errno\":22"));
    free(invalid);
    uint64_t reader = open_file(session, 0);
    unsigned char buffer[4096];
    int64_t count = -1;
    assert(fvs_read(session, reader, buffer, sizeof(buffer), 101, &count) == 0);
    assert(count == sizeof(buffer));
    for (size_t i = 0; i < sizeof(buffer); i++) assert(buffer[i] == (unsigned char)((101 + i) % 251));
    assert(fvs_read(session, reader, buffer, sizeof(buffer), INT64_MAX, &count) == 0 && count == 0);
    assert(fvs_read(session, reader, NULL, 1, 0, &count) == EINVAL);
    assert(fvs_read(session, reader, buffer, -1, 0, &count) == EINVAL);
    assert(fvs_read(session, reader, buffer, 1, -1, &count) == EINVAL);
    uint64_t writer = open_file(session, 1);
    assert(fvs_write(session, writer, "UP", 2, 37, &count) == 0 && count == 2);
    assert(fvs_read(session, reader, buffer, 2, 37, &count) == 0 && memcmp(buffer, "UP", 2) == 0);
    char *reply = fvs_call(session, "{\"op\":\"sync\"}");
    assert(reply && strstr(reply, "\"errno\":0")); free(reply);
    struct timespec start, end;
    assert(clock_gettime(CLOCK_MONOTONIC, &start) == 0);
    const int iterations = 10000;
    for (int i = 0; i < iterations; i++) {
        assert(fvs_read(session, reader, buffer, sizeof(buffer), (i * 7919) % (4 * 1024 * 1024 - 4096), &count) == 0);
        assert(count == sizeof(buffer));
    }
    assert(clock_gettime(CLOCK_MONOTONIC, &end) == 0);
    double ns = (end.tv_sec - start.tv_sec) * 1e9 + end.tv_nsec - start.tv_nsec;
    printf("C ABI upper random 4KiB: %.0f ns/read (%d iterations)\n", ns / iterations, iterations);
    assert(fvs_close(session) == 0);
    assert(fvs_close(session) == EBADF);
    assert(fvs_read(session, reader, buffer, 1, 0, &count) == EBADF);
    puts("C ABI lifecycle, offsets, EOF, copy-up, binary writes: PASS");
    return 0;
}
