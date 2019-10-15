set -e

echo "BUILDING PROTO FILES"

mkdir output

protoc mount/**/*.proto \
  -Imount/ \
  -I/usr/include/ \
  --go_out=plugins=grpc:output

cp -r -f output/open-match.dev/open-match-ecosystem/* mount/
