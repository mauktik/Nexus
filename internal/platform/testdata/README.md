# os-release fixtures

Each file is the unmodified `/etc/os-release` of an official Docker Hub image,
captured with `docker run --rm <image> cat /etc/os-release`:

| File | Image | Image ID |
|---|---|---|
| `os-release.ubuntu` | `ubuntu:24.04` | `sha256:fe81560990fae86dc4743202b2cb4d3df98d0bcf949173cecef15ce1127ef852` |
| `os-release.debian` | `debian:13.7-slim` | `sha256:a29215f6a35e51e22adffa17f89e9d2ef06214e64a2bad10d765c46aea49f11f` |
| `os-release.alpine` | `alpine:3.21` | `sha256:bc21487101ae47172509130ee1092cc273a831968d64b617776d1f69c81e42ab` |
