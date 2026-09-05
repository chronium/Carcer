/* Opaque byte-copy fixture demonstrates Linux converters, no guest format. */
#include <stdio.h>
int main(int argc, char **argv) {
    if (argc != 3) return 2;
    FILE *in = fopen(argv[1], "rb"), *out = fopen(argv[2], "wb");
    if (!in || !out) return 3;
    char buffer[4096];
    size_t n;
    while ((n = fread(buffer, 1, sizeof buffer, in)))
        if (fwrite(buffer, 1, n, out) != n) return 4;
    int failed = ferror(in);
    failed |= fclose(in);
    failed |= fclose(out);
    return failed != 0;
}
