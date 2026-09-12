# gdrive-s3

An S3-compatible object storage API backed by Google Drive. Point any AWS S3
client (aws-cli, boto3, s3fs, the AWS SDKs) at it and use Google Drive as an
object store.

It speaks the S3 REST protocol directly — AWS Signature V4 authentication,
`aws-chunked` streaming uploads, multipart uploads, range requests, and the
usual XML responses — so existing tooling works without changes.

## Features

- **Drop-in S3 API**: buckets (`ListBuckets`, `CreateBucket`, `HeadBucket`,
  `DeleteBucket`) and objects (`PutObject`, `GetObject`, `HeadObject`,
  `DeleteObject`, `ListObjectsV2` with prefix/delimiter/pagination).
- **Multipart uploads** for large objects, mapped to a staging area on Drive.
- **Range requests** (`Range: bytes=…`), including on encrypted objects.
- **SigV4** authentication verified server-side.
- **Multi-user**: each user signs in with Google OAuth and receives an S3 access
  key / secret pair scoped to their own Drive.
- **Optional at-rest encryption**: a passphrase enables transparent, streaming
  authenticated encryption (XSalsa20-Poly1305, key derived with scrypt). Google
  Drive only ever stores ciphertext; the passphrase never leaves the operator.
- **Web console**: a built-in single-page UI to browse buckets and objects,
  upload (drag and drop), download and delete, with a copy-paste snippet for the
  equivalent S3 client commands. It shares the root path with the S3 API and is
  served to browsers, while signed S3 requests are routed to the protocol layer.
- Uses the narrow `drive.file` OAuth scope (the app only sees files it created).

## Quick start

```bash
# 1. Configure (env vars or a .env file)
export GOOGLE_CLIENT_ID=...  GOOGLE_CLIENT_SECRET=...   # OAuth application
export LISTEN_ADDR=127.0.0.1:9000  ROOT_FOLDER=gdrive-s3
# optional at-rest encryption:
export ENCRYPTION_PASSPHRASE="correct horse battery staple"

# 2. Run
go run ./cmd/gdrive-s3

# 3. Onboard: open http://127.0.0.1:9000/auth/login, sign in with Google,
#    and copy the access key / secret key you are shown.

# 4. Use it with any S3 client
export AWS_ACCESS_KEY_ID=... AWS_SECRET_ACCESS_KEY=... AWS_DEFAULT_REGION=us-east-1
aws configure set default.s3.addressing_style path
EP="--endpoint-url http://127.0.0.1:9000"

aws $EP s3 mb s3://photos
aws $EP s3 cp ./movie.mp4 s3://photos/movie.mp4     # multipart for large files
aws $EP s3 cp s3://photos/movie.mp4 ./out.mp4
aws $EP s3 ls s3://photos/
```

For quick local use without the OAuth flow, seed one account from configuration
with `GOOGLE_REFRESH_TOKEN`, `S3_ACCESS_KEY` and `S3_SECRET_KEY`.

### Web console

Open the root URL in a browser. Signing in with Google establishes a session and
lets you manage your own buckets and objects; without a session the console is
read-only. Set `DEMO_ACCESS_KEY` to the access key of an onboarded account to
expose that account as a public, read-only demo (visitors can browse and
download but not create, upload or delete).

## Architecture

```
cmd/gdrive-s3        entrypoint, wiring
internal/config      configuration (env / .env)
internal/gdrive      Google Drive v3 client (OAuth refresh, files, folders, ranges)
internal/storage     S3 object model over Drive (buckets, objects, multipart, listing)
internal/crypt       streaming authenticated encryption (secretbox + scrypt)
internal/users       persistent access-key -> account store
internal/account     resolves an access key to a per-user storage backend
internal/auth        Google OAuth sign-in flow
internal/console     browser UI and its session-authenticated JSON API
internal/s3          S3 protocol: SigV4 verification, XML, HTTP handlers
```

### Notes

- A bucket is a Drive folder; an object is a file whose name is the key (S3 uses
  a flat keyspace). The object ETag is Drive's MD5 checksum (opaque for encrypted
  objects).
- Multipart parts are staged as files under a hidden `.uploads` folder and
  concatenated on completion.
- Encryption is per-server (one passphrase). Enabling it on a store that already
  holds plaintext, or vice versa, is unsupported.
- The `/auth/` and `/console/` path prefixes are reserved for the sign-in flow
  and the console API; avoid bucket names that would collide with them.

## License

MIT
