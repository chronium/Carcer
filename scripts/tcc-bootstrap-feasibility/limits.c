#define _POSIX_C_SOURCE 200809L
#include <errno.h>
#include <signal.h>
#include <stdio.h>
#include <stdlib.h>
#include <string.h>
#include <sys/wait.h>
#include <time.h>
#include <unistd.h>
int main(int argc, char **argv) {
    if (argc != 2) return 2;
    if (!strcmp(argv[1], "pids")) {
        pid_t children[80]; int n = 0, failed = 0;
        for (; n < 80; n++) {
            pid_t pid = fork();
            if (pid < 0) { failed = errno == EAGAIN; break; }
            if (!pid) { sleep(10); _exit(0); }
            children[n] = pid;
        }
        for (int i = 0; i < n; i++) kill(children[i], SIGKILL);
        for (int i = 0; i < n; i++) waitpid(children[i], NULL, 0);
        printf("forked=%d pids_limit_enforced=%d\n", n, failed);
        return !failed;
    }
    if (!strcmp(argv[1], "memory")) {
        for (int i = 0; i < 128; i++) {
            volatile unsigned char *p = malloc(1024 * 1024);
            if (!p) return 3;
            for (int j = 0; j < 1024 * 1024; j += 4096) p[j] = 1;
        }
        return 4; /* Reaching 128 MiB means the 64 MiB cap did not work. */
    }
    if (!strcmp(argv[1], "cpu")) {
        for (int i = 0; i < 4; i++) {
            pid_t pid = fork();
            if (pid < 0) return 3;
            if (!pid) {
                time_t end = time(NULL) + 3;
                volatile unsigned long n = 0;
                while (time(NULL) < end) n++;
                _exit(0);
            }
        }
        while (wait(NULL) > 0) {}
        return 0;
    }
    return 2;
}
