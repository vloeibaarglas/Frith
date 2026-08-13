# Frith

Frith — a minimal self-hosted image/media server. Go stdlib-only, single binary, serve + token-authenticated upload.

![CI](https://github.com/vloeibaarglas/Frith/actions/workflows/ci.yml/badge.svg)
![Release](https://img.shields.io/github/v/release/vloeibaarglas/Frith)
![Go](https://img.shields.io/badge/Go-1.24-blue)
![License](https://img.shields.io/github/license/vloeibaarglas/Frith)

## Quick start

```bash
# Docker
git clone https://github.com/vloeibaarglas/Frith
cd frith
cp .env.example .env        # set UPLOAD_TOKEN
docker compose up -d

# or a prebuilt binary
UPLOAD_TOKEN='secret' ./frith -data ./uploads

# or from source
go build -o frith ./cmd/frith
```

## Configuration

Everything is configurable via env vars or flags (flags win).

| Env            | Flag       | Default          | Meaning                                    |
|----------------|------------|------------------|--------------------------------------------|
| `UPLOAD_ADDR`  | `-addr`    | `:8080`          | Listen address                             |
| `UPLOAD_DIR`   | `-data`    | `./uploads`      | Where files are stored                     |
| `UPLOAD_TOKEN` | —          | *required*       | Comma-separated upload keys                |
| `PUBLIC_URL`   | `-url-base`| request host     | Base URL used in upload responses          |
| `UPLOAD_PATH`  | `-path`    | `/`              | URL prefix files are served under (e.g. `/u`) |
| `UPLOAD_MAX_MB`| `-max-mb`  | `500`            | Max single-file upload size (MB)           |
| `UPLOAD_ALLOW` | `-allow`   | media set        | Comma-separated allow-list of extensions; **empty string = allow all** |
| `UPLOAD_DENY`  | `-deny`    | *none*           | Comma-separated deny-list of extensions    |
| `UPLOAD_LIST`  | `-list`    | `false`          | Enable token-authenticated `GET /api/files` inventory |
| `UPLOAD_ORIGINAL_NAME` | `-original-name` | `false` | Capture & serve original filenames (sidecar metadata) |

Files are served at `{UPLOAD_PATH}/{name}` (default `/`). A file is accepted when its extension is **not denied** and (if the allow-list is non-empty) **is in the allow-list**.

## Upload

```bash
curl -X POST https://host/api/upload \
  -H "Authorization: Bearer $UPLOAD_TOKEN" \
  -F "file=@photo.png"
# => {"files":[{"url":"https://host/x9F2aB.png"}]}
```

## Deploying

Frith is a plain origin server — put any reverse proxy or CDN in front of it. Its `Cache-Control` headers drive edge caching (30 days for media, `no-store` for HTML), so no cache rules are needed.

## Related

- [Frith Uploader](https://github.com/vloeibaarglas/Frith-Uploader) — Android app for uploading to Frith

## License

[MIT](https://github.com/vloeibaarglas/Frith/blob/main/LICENSE)
