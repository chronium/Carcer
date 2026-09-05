#include <stdio.h>
struct pair { int first, second; };
_Static_assert(sizeof(int) >= 4, "int width");
int main(void) {
    struct pair p = {.second = 23, .first = 19};
    int n = p.first + p.second;
    int vla[n];
    vla[n - 1] = n;
    printf("native-c99-c11-smoke=%d\n", vla[n - 1]);
    return vla[n - 1] != 42;
}
