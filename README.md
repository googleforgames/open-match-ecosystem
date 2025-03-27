# open-match-ecosystem

Anything we provide that is not the core functionality of Open Match but interacts with it can be found here, along with a few other odds and ends that don't fit elsewhere.

## Open Match v1.x
Demos, examples, and tests, oh my!

Command to rebuild all of the proto files in the repository:
```sh
docker build -f=Dockerfile.build-protos -t build-protos . && docker run --rm --mount type=bind,source="$(pwd)",target=/workdir/mount build-protos
```
## Open Match v2.x

See the `v2` directory for code intended to work with Open Match 2.x.
