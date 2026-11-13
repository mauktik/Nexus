# test/buildsys/testdata

## dynamic.elf

A tiny dynamically linked ELF (6064 bytes) for TC-5: the static-binary checker
(`test/buildsys/staticcheck`) must reject it. It has a `PT_INTERP` program
header (`/lib/ld-linux-aarch64.so.1`) and one `DT_NEEDED` entry, `libc.so.6`.
It is never executed, so its architecture (aarch64) does not matter.

Built with gcc 14.2.0 (Debian 14.2.0-19) in the `rust:1.93.1-slim-trixie`
image (linux/arm64), from a C file holding only `int main(void) { return 0; }`:

    docker run --rm -i rust:1.93.1-slim-trixie sh -c '
        cd /tmp &&
        printf "int main(void) { return 0; }\n" >dynamic.c &&
        gcc -Os -s -Wl,-z,noseparate-code -Wl,-z,max-page-size=0x1000 \
            -Wl,--build-id=none -o dynamic.elf dynamic.c >&2 &&
        cat dynamic.elf' >dynamic.elf

SHA-256 of the committed file:
`8042463b52c2afbe669019247a032caedecbd8405cc08a65fcc803b233528840`.

`readelf -lW` reports `[Requesting program interpreter:
/lib/ld-linux-aarch64.so.1]`, and `readelf -dW` reports `(NEEDED) Shared
library: [libc.so.6]`.
