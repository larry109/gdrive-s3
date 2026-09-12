# gdrive-s3

An S3-compatible object storage API backed by Google Drive. Point any AWS S3
client (aws-cli, boto3, s3fs, the AWS SDKs) at it and use your Google Drive as
an object store.

It speaks the S3 REST protocol directly — AWS Signature V4 authentication,
`aws-chunked` streaming uploads, and the usual XML responses — so existing
tooling works without changes.

## Features

- **Drop-in S3 API**: `ListBuckets`, `CreateBucket`, `DeleteBucket`,
  `HeadBucket`, `PutObject`, `GetObject`, `HeadObject`, `DeleteObject`,
  `ListObjectsV2` (prefix, delimiter, pagination).
- **SigV4 auth** verified server-side from an access key / secret pair.
- **Streaming uploads**: decodes the `aws-chunked` body the AWS CLI sends.
- **Drive mapping**: a bucket is a folder, an object is a file whose name is the
  key; the object ETag is Drive's MD5 checksum.
- Uses the narrow `drive.file` OAuth scope (the app only ever sees files it
  created).

## Quick start

```bash
# 1. Configure (env vars or a .env file)
export GOOGLE_CLIENT_ID=...        GOOGLE_CLIENT_SECRET=...
export GOOGLE_REFRESH_TOKEN=...    # drive.file scope
export S3_ACCESS_KEY=mykey         S3_SECRET_KEY=mysecret
export LISTEN_ADDR=127.0.0.1:9000  ROOT_FOLDER=gdrive-s3

# 2. Run
go run ./cmd/gdrive-s3

# 3. Use it with the AWS CLI
export AWS_ACCESS_KEY_ID=mykey AWS_SECRET_ACCESS_KEY=mysecret AWS_DEFAULT_REGION=us-east-1
aws configure set default.s3.addressing_style path
EP="--endpoint-url http://127.0.0.1:9000"

aws $EP s3 mb s3://photos
aws $EP s3 cp ./cat.jpg s3://photos/2024/cat.jpg
aws $EP s3 ls s3://photos/2024/
aws $EP s3 cp s3://photos/2024/cat.jpg ./out.jpg
```

## Architecture

```
cmd/gdrive-s3        entrypoint, wiring
internal/config      configuration (env / .env)
internal/gdrive      Google Drive v3 client (OAuth refresh, files, folders)
internal/storage     S3 object model over Drive (buckets, objects, listing)
internal/s3          S3 protocol: SigV4 verification, XML, HTTP handlers
```

## Roadmap

- Multipart uploads for large objects (mapped to Drive resumable sessions).
- Per-user Google OAuth: each user connects their own account and gets an S3
  key pair scoped to their Drive.
- Optional client-transparent, zero-knowledge encryption (XSalsa20-Poly1305,
  key derived from a user passphrase).
- Range requests on `GetObject`.

## License

MIT
