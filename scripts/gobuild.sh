#!/bin/bash -x

# Go module now at repo root - build from root
cd ..
VERSION="nixy-build-$(date +%Y%m%d%H%M%S)"
go build -ldflags="-X \"main.version=${VERSION}\" -X 'main.date=$(date +"%Y-%m-%d %H:%M:%S")' -X 'main.commit=$(git rev-parse --abbrev-ref HEAD)-$(git rev-parse HEAD)'" -o nixy .
if [ "$?" -ne 0 ]; then
    echo "Build failure"
    exit 1
fi

echo "Build version details:"
cp ./nixy ./scripts/

pushd ./docker
cp ../scripts/nixy .
docker build -t ghcr.io/phonepe/drove-gateway:${VERSION} -t  ghcr.io/phonepe/drove-gateway:latest .
popd
