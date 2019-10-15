# open-match-ecosystem
Demos, examples, and tests, oh my!

Command to rebuild all of the proto files in the repository:
```sh
docker build -f=Dockerfile.build-protos -t build-protos . && docker run --rm --mount type=bind,source="$(pwd)",target=/workdir/mount build-protos
```
