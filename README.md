# frith

A minimal, self-hosted media server. Serves images, HTML, gif, mp4, PDFs — anything you upload — from a flat directory with token-authenticated uploads. Built entirely on the Go standard library: **zero dependencies, one static binary (~5 MB), ~5 MB RAM**.

No database, no dashboard, no accounts, no stats. Just serve and upload. Put Cloudflare (or nginx) in front if you want caching and a public hostname.

## Features

- **Serves anything** with correct MIME types: `png jpg gif svg webp avif mp4 webm mov pdf html txt md json csv …`
- **Byte-range support** (`Accept-Ranges`) — mp4/video seeking and streaming work out of the box.
- **Upload via multipart or raw body**, Bearer-token auth (`Authorization: Bearer <key>`).
- **Original filenames** (opt-in) — saved as sidecar metadata, served via `Content-Disposition`, so "Save As" keeps your name; `?download` forces attachment.
- **No memory blowups** — uploads stream to disk, so large files are fine.
- **Configurable size cap and extension allow/deny lists** — env or flags.
- Crypto-random 6-char names (base62), no dotfiles served.

## Quick start

### Docker (recommended)

```bash
git clone https://github.com/vloeibaarglas/Frith
cd frith
cp .env.example .env        # edit UPLOAD_TOKEN
docker compose up -d
```

Prebuilt images are published to `ghcr.io/vloeibaarglas/frith` on every `v*` tag.

### Prebuilt binary

```bash
# grab frith-linux-amd64 from the latest release
chmod +x frith-linux-amd64
UPLOAD_TOKEN='secret' ./frith-linux-amd64 -data ./uploads -addr :8080
```

### From source

```bash
go build -o frith ./cmd/frith   # Go 1.24+
UPLOAD_TOKEN='secret' ./frith -data ./uploads
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

Files are served at `{UPLOAD_PATH}/{name}` — the default `/` means `https://host/x9F2aB.png`; setting `UPLOAD_PATH=/u` gives `https://host/u/x9F2aB.png` (upload responses use the same prefix).

A file is accepted when its extension is **not denied** and (if the allow-list is non-empty) **is in the allow-list**. The default allow-list is a safe media set (`png jpg jpeg gif svg webp avif mp4 webm mov m4v pdf html htm txt md json csv css js xml zip gz`). To allow everything except a few types:

```bash
UPLOAD_ALLOW="" UPLOAD_DENY="sh,exe,php,jar" ./frith
```

## API

### Upload a file

```bash
# multipart (one or more "file" fields)
curl -X POST https://media.example.com/api/upload \
  -H "Authorization: Bearer $UPLOAD_TOKEN" \
  -F "file=@photo.png"

# raw body with explicit extension
curl -X POST https://media.example.com/api/upload?ext=png \
  -H "Authorization: Bearer $UPLOAD_TOKEN" \
  --data-binary @photo.png

# raw body, with an original filename (only meaningful with -original-name)
curl -X POST "https://media.example.com/api/upload?ext=png&name=quarterly-report.png" \
  -H "Authorization: Bearer $UPLOAD_TOKEN" \
  --data-binary @photo.png
```

Response:

```json
{
  "files": [
    {
      "id": "x9F2aB.png",
      "name": "x9F2aB.png",
      "type": "image/png",
      "size": 43885,
      "url": "https://media.example.com/x9F2aB.png",
      "originalName": "photo.png"
    }
  ]
}
```

`originalName` is only present when `UPLOAD_ORIGINAL_NAME=true` and the upload carried a filename (multipart's filename, or `?name=` for raw body).

Errors: `401` bad/missing token · `413` over size limit · `415` disallowed extension.

### List files (opt-in)

With `UPLOAD_LIST=true` (or `-list`), `GET /api/files` returns a JSON inventory of everything in the data dir — name, MIME type, size, and modification date (newest first). Requires the same `Authorization: Bearer <key>` header as uploads.

```bash
curl -H "Authorization: Bearer $UPLOAD_TOKEN" https://media.example.com/api/files
# => {"files":[{"name":"x9F2aB.png","type":"image/png","size":43885,"date":"2026-06-10T05:28:05Z","originalName":"photo.png"}, ...]}
```

### Serve a file

```bash
curl https://media.example.com/x9F2aB.png     # default path (/)
curl https://media.example.com/u/x9F2aB.png   # if UPLOAD_PATH=/u
curl -OJ "https://media.example.com/x9F2aB.png?download"   # force download, keep original name
```

Files are streamed with `Content-Type`, `Cache-Control`, `Accept-Ranges` and `Last-Modified` set for you. HTML is served inline (`text/html`) so it renders as a page. With `UPLOAD_ORIGINAL_NAME=true`, the original filename is sent via `Content-Disposition` (RFC 5987 `filename*`), so browsers and `curl -OJ` save it under the name you uploaded.

## Deploying behind Cloudflare / nginx

frith is a plain origin server — put any reverse proxy or CDN in front. Its own `Cache-Control` headers drive edge caching (30 days for media, `no-store` for HTML), so no cache rules are needed. Set `PUBLIC_URL` to your public hostname so responses return the right links.

Example nginx server block:

```nginx
server {
    listen 443 ssl;
    server_name media.example.com;
    client_max_body_size 0;   # let the app enforce the limit
    location / {
        proxy_pass http://127.0.0.1:8080;
        proxy_request_buffering off;   # stream large uploads
        proxy_set_header X-Forwarded-Proto $scheme;
    }
}
```

## Security notes

- The **only** public endpoints are file serving and `/api/healthcheck`. Everything else 404s.
- Uploads require a token; tokens are compared in constant time. Trust your users — there is deliberately no per-file password/max-views/expiry.
- Files are served exactly as uploaded. HTML files render inline on the same origin as the upload API; keep the server on a hostname with no cookies/authentication if you host untrusted HTML.
- Dotfiles (`.env`, etc.) in the data dir are never served.

## License

MIT
