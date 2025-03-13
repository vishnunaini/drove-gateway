#!/bin/bash -x

pushd ../drove-gateway
VERSION="drove-gateway-build-$(date +%Y%m%d%H%M%S)"
go build -ldflags="-X \"main.version=${VERSION}\" -X 'main.date=$(date +"%Y-%m-%d %H:%M:%S")' -X 'main.commit=$(git rev-parse --abbrev-ref HEAD)-$(git rev-parse HEAD)'" -o drove-gateway .
if [ "$?" -ne 0 ]; then
    echo "Build failure"
fi

popd
mv ../drove-gateway/drove-gateway ../bin/drove-gateway
echo "Build version details: ${VERSION}"

pushd ../docker
cp ../bin/drove-gateway .
docker build -t ghcr.io/phonepe/drove-gateway:${VERSION} -t  ghcr.io/phonepe/drove-gateway:latest .
popd
