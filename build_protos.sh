set -e

echo "BUILDING PROTO FILES"

echo "MEMEME"
# ls protoc/bin/
echo "MEMEME"

mkdir output

# ./protoc/bin/protoc mount/**/*.proto \
protoc mount/**/*.proto \
  -Imount/ \
  -I/usr/include/ \
  --go_out=plugins=grpc:output

ls output/open-match.dev/open-match-ecosystem

cp -r -f output/open-match.dev/open-match-ecosystem/* mount/

# ls protoc/bin -l
# /workdir/protoc/bin/protoc --help # *.proto \
#   # --go_count=plugins=grpc:output

echo "HERE"
ls output/open-match.dev/open-match-ecosystem/protoexample
echo "HERE"
echo "THERE"
ls mount/protoexample
echo "THERE"

# PROTOC_VERSION = 3.8.0

# https://github.com/protocolbuffers/protobuf/releases/download/v$(PROTOC_VERSION)/protoc-$(PROTOC_VERSION)-linux-x86_64.zip

#build/toolchain/bin/protoc$(EXE_EXTENSION):
#  mkdir -p $(TOOLCHAIN_BIN)
#  curl -o $(TOOLCHAIN_DIR)/protoc-temp.zip -L $(PROTOC_PACKAGE)
#  (cd $(TOOLCHAIN_DIR); unzip -q -o protoc-temp.zip)
#  rm $(TOOLCHAIN_DIR)/protoc-temp.zip $(TOOLCHAIN_DIR)/readme.txt

#RUN echo "something \
#another \
#line" > test.sh

#RUN cat test.sh

# need build/toolchain/bin/protoc$(EXE_EXTENSgit ION) build/toolchain/bin/protoc-gen-go$(EXE_EXTENSION) 

#  mkdir -p $(REPOSITORY_ROOT)/build/prototmp $(REPOSITORY_ROOT)/pkg/pb
#  $(PROTOC) $< \
#    -I $(REPOSITORY_ROOT) -I $(PROTOC_INCLUDES) \
#    --go_out=plugins=grpc:$(REPOSITORY_ROOT)/build/prototmp
#  mv $(REPOSITORY_ROOT)/build/prototmp/open-match.dev/open-match/$@ $@

# PROTOC_INCLUDES := $(REPOSITORY_ROOT)/third_party

#third_party/google/api:
#  mkdir -p $(TOOLCHAIN_DIR)/googleapis-temp/
#  mkdir -p $(REPOSITORY_ROOT)/third_party/google/api
#  mkdir -p $(REPOSITORY_ROOT)/third_party/google/rpc
#  curl -o $(TOOLCHAIN_DIR)/googleapis-temp/googleapis.zip -L https://github.com/googleapis/googleapis/archive/master.zip
#  (cd $(TOOLCHAIN_DIR)/googleapis-temp/; unzip -q -o googleapis.zip)
#  cp -f $(TOOLCHAIN_DIR)/googleapis-temp/googleapis-master/google/api/*.proto $(REPOSITORY_ROOT)/third_party/google/api/
#  cp -f $(TOOLCHAIN_DIR)/googleapis-temp/googleapis-master/google/rpc/*.proto $(REPOSITORY_ROOT)/third_party/google/rpc/
#  rm -rf $(TOOLCHAIN_DIR)/googleapis-temp
