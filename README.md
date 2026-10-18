# goStatic [![Docker Pulls](https://img.shields.io/docker/pulls/pierrezemb/gostatic.svg?style=plastic)](https://hub.docker.com/r/pierrezemb/gostatic/) [![Docker Build](https://img.shields.io/docker/build/pierrezemb/gostatic.svg?style=plastic)](https://hub.docker.com/r/pierrezemb/gostatic/) [![GoDoc](https://godoc.org/github.com/PierreZ/goStatic?status.svg)](https://godoc.org/github.com/PierreZ/goStatic)
A really small, multi-arch, static web server for Docker

## The goal
My goal is to create to smallest docker container for my web static files. The advantage of Go is that you can generate a fully static binary, so that you don't need anything else.

### Wait, I've been using old versions of GoStatic and things have changed!

Yeah, decided to drop support of unsecured HTTPS. Two-years ago, when I started GoStatic, there was no automatic HTTPS available. Nowadays, thanks to Let's Encrypt, it's really easy to do so. If you need HTTPS, I recommend [caddy](https://caddyserver.com).

## Features
 * A fully static web server embedded in a `SCRATCH` image
 * No framework
 * Web server built for Docker
 * Light container
 * More secure than official images (see below)
 * Log enabled
 * Specify custom response headers per path and filetype [(info)](./docs/header-config.md) 

## Why?
Because the official Golang image is wayyyy too big (around 1/2Gb as you can see below) and could be insecure.

[![](https://badge.imagelayers.io/golang:latest.svg)](https://imagelayers.io/?images=golang:latest 'Get your own badge on imagelayers.io')

For me, the whole point of containers is to have a light container...
Many links should provide you with additional info to see my point of view:

 * [Over 30% of Official Images in Docker Hub Contain High Priority Security Vulnerabilities](http://www.banyanops.com/blog/analyzing-docker-hub/)
 * [Create The Smallest Possible Docker Container](http://blog.xebia.com/2014/07/04/create-the-smallest-possible-docker-container/)
 * [Building Docker Images for Static Go Binaries](https://medium.com/@kelseyhightower/optimizing-docker-images-for-static-binaries-b5696e26eb07)
 * [Small Docker Images For Go Apps](https://www.ctl.io/developers/blog/post/small-docker-images-for-go-apps)

## How to use
```
docker run -d -p 80:8043 -v path/to/website:/srv/http --name goStatic pierrezemb/gostatic
```

This fork publishes its own images to the GitHub Container Registry on every
push to `main`, tagged `latest` and with the commit sha:

```
docker run -d -p 80:8043 -v path/to/website:/srv/http --name goStatic ghcr.io/smit-io/gostatic
```

## Usage 

```
./goStatic --help
Usage of ./goStatic:
  -append-header HeaderName:Value
        HTTP response header, specified as HeaderName:Value that should be added to all responses.
  -context string
        The 'context' path on which files are served, e.g. 'doc' will serve the files at 'http://localhost:<port>/doc/'
  -default-user-basic-auth string
        Define the user (default "gopher")
  -enable-basic-auth
        Enable basic auth. By default, password are randomly generated. Use --set-basic-auth to set it.
  -enable-gzip
        Compress responses with gzip for clients that accept it. Payloads that are already compressed (images, archives, fonts) are served as-is.
  -enable-health
        Enable health check endpoint. You can call /health to get a 200 response. Useful for Kubernetes, OpenFaas, etc.
  -enable-logging
        Enable log request
  -fallback string
        Default fallback file. Either absolute for a specific asset (/index.html), or relative to recursively resolve (index.html)
  -header-config-path string
        Path to the config file for custom response headers (default "/config/headerConfig.json")
  -https-promote
        All HTTP requests should be redirected to HTTPS
  -password-length int
        Size of the randomized password (default 16)
  -path string
        The path for the static files (default "/srv/http")
  -port int
        The listening port (default 8043)
  -set-basic-auth string
        Define the basic auth. Form must be user:password
```

The `/ip` endpoint returns the requesting client's IP address as plain text.

The `/dns` endpoint performs a reverse DNS lookup for that same address and
returns the first PTR record as plain text. 

The `/json` endpoint returns a JSON object with `dns` and `ip` keys containing the corresponding endpoint
responses.

### Fallback

The fallback option is principally useful for single-page applications (SPAs) where the browser may request a file, but where part of the path is in fact an internal route in the application, not a file on disk. goStatic supports two possible usages of this option:

1. Using an absolute path so that all not found requests resolve to the same file
2. Using a relative file, which searches up the tree for the specified file

The second case is useful if you have multiple SPAs within the one filesystem. e.g., */* and */admin*.

### Compression

Pass `--enable-gzip` to compress responses for clients that send
`Accept-Encoding: gzip`. Files whose payloads are already compressed (images,
video, archives, fonts, PDFs) are served uncompressed, since gzipping them
costs CPU and usually makes the response slightly larger. Responses carry
`Vary: Accept-Encoding` so shared caches store the two variants separately.

> **Behaviour change:** compression used to be applied only as a side effect of
> passing `--append-header`, and could not be enabled on its own. It is now
> controlled solely by `--enable-gzip`. If you relied on `--append-header` to
> compress responses, add `--enable-gzip`; goStatic logs a warning at startup
> when `--append-header` is used without it.


## Development

`make help` lists every target. The Go targets need only a Go toolchain; the
rest need a running Docker daemon.

```bash
make check           # gofmt, go vet and the tests under -race — what CI checks
make run             # build and serve testdata/www on http://localhost:8043
make cover           # write coverage.html
make docker-build    # single-arch image for this machine, tagged gostatic:dev
make docker-build-all # all eight release platforms, exactly as CI builds them
```

Override any variable on the command line, e.g. `make run PORT=9000
WWW=/path/to/site` or `make docker-build TAG=v1`.

### Publishing images locally

`make publish-local` starts a throwaway registry, builds a multi-arch image and
pushes it there, without involving GHCR:

```bash
make publish-local              # push 127.0.0.1:5001/gostatic:dev
make publish-local-check        # show the resulting manifest list
docker pull 127.0.0.1:5001/gostatic:dev
make registry-down builder-down # tear it all down
```

The registry is published on port 5001 because AirPlay Receiver occupies 5000
on macOS. The buildx builder reaches it by container name over a shared docker
network, since a published port is not visible from inside the builder on
Docker Desktop; `hack/buildkitd.toml` allows plain HTTP for that hostname.

### Running the workflows locally

[act](https://github.com/nektos/act) runs the GitHub Actions workflows on your
own machine:

```bash
make ci-list   # list the jobs act would run
make ci-dry    # walk the build workflow without executing any step
make ci        # actually run the build workflow
```

Both targets are scoped to `build.yml` deliberately: `docker-push.yml` publishes
to GHCR, which a local run has no business attempting. Use `make publish-local`
instead when you want an image out of a local build.

`make ci` runs the whole build job, image build included, against your local
Docker daemon, so it reuses your build cache — roughly a minute warm, longer on
a cold cache.

## Build

### Docker images
```bash
docker buildx create --use --name=cross
docker buildx build --platform=linux/amd64,linux/arm64,linux/arm/v5,linux/arm/v6,linux/arm/v7,darwin/amd64,darwin/arm64,windows/amd64 .
```