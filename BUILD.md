# Building Drove Gateway Packages

This document describes how to build Debian (.deb) and RPM (.rpm) packages for drove-gateway.

## Overview

Drove Gateway supports packaging for multiple Linux distributions:
- **Debian/Ubuntu**: Using debhelper and debian/rules
- **RHEL 9/CentOS 9/AlmaLinux 9**: Using rpmbuild and rpmbuild/SPECS/drove-gateway.spec

## Building Debian Packages

### Prerequisites
```bash
sudo apt-get install -y \
  build-essential \
  debhelper-compat \
  golang-go \
  git \
  devscripts
```

### Build (Official Debian Method)
  debhelper-compat \
  golang-go \

```bash
# Build binary package only (faster)
dpkg-buildpackage -us -uc -b

# Build source and binary packages
dpkg-buildpackage -us -uc

# Build using helper script (wrapper around dpkg-buildpackage)
./scripts/debbuild.sh        # Binary only (default)
./scripts/debbuild.sh -s     # Source package
```

### Go Version
Go 1.23 or newer is required. On Ubuntu 20.04 (focal), add the longsleep/golang-backports PPA:
```bash
sudo apt-get install -y software-properties-common
sudo add-apt-repository ppa:longsleep/golang-backports
sudo apt-get update
sudo apt-get install -y golang-go
### Output
- Binary package: `../drove-gateway_*.deb`
- Source package: `../drove-gateway_*.dsc` (if -s used)
- Build logs: `../drove-gateway_*.build`

### Installation
```bash
# Install and automatically resolve dependencies
sudo apt-get install ./drove-gateway_*.deb

# Or manual installation
sudo dpkg -i drove-gateway_*.deb
sudo apt-get install -f  # Install any missing dependencies

# Enable and start service
sudo systemctl enable drove.gateway.service
sudo systemctl start drove.gateway.service
### Installation
```bash
# Install and automatically resolve dependencies
sudo apt-get install ./drove-gateway_*.deb

# Or manual installation
sudo dpkg -i drove-gateway_*.deb
sudo apt-get install -f  # Install any missing dependencies

# Service management is handled by deb-systemd-helper in maintainer scripts.
# To manually check or control the service:
sudo systemctl status drove.gateway.service
sudo systemctl start drove.gateway.service
sudo systemctl enable drove.gateway.service
```

# Verify installation
sudo systemctl status drove.gateway.service
```

### Verification
```bash
# Check package contents
dpkg -c drove-gateway_*.deb

# Get package information
dpkg -I drove-gateway_*.deb

# Run Debian linting checks
lintian drove-gateway_*.deb
```
### RPM Build Workflow
**Trigger**: Push to `main`, Tags matching `v*`, Manual trigger
**Workflow**: `.github/workflows/rpm-build.yml`

Builds RPM packages in UBI9/UBI10 container and uploads artifacts.

### Debian Release Workflow
**Trigger**: Git tags matching `v*`
**Workflow**: `.github/workflows/debian-release.yml`

### RPM Release Workflow
**Trigger**: Git tags matching `v*`
**Workflow**: `.github/workflows/rpm-release.yml`

Creates GitHub releases with built packages and checksums using official tools:
- Debian: `dpkg-buildpackage`
- RPM: `rpmbuild`

## Building RPM Packages

### Prerequisites
```bash
sudo dnf install -y \
  gcc \
  golang \
  systemd-devel \
  rpm-build \
  rpmdevtools \
  redhat-rpm-config
```

### Build

#### Option 1: Using rpmbuild directly
```bash
# Setup rpmbuild directory structure
rpmdev-setuptree

# Copy spec file
cp rpmbuild/SPECS/drove-gateway.spec ~/rpmbuild/SPECS/

# Create source tarball
tar --exclude='.git' --exclude='.github' --exclude='rpmbuild' \
**Error**: `go: cannot find GOROOT`
- Set Go path: `export PATH=$PATH:/usr/local/go/bin`
- On Ubuntu 20.04, ensure you have the longsleep/golang-backports PPA and Go >= 1.23

# Build package
**Error**: `systemd headers not found`
- Install systemd-devel: `sudo dnf install systemd-devel`
```

#### Option 2: Using provided script (if available)
```bash
bash scripts/rpmbuild.sh
```

### Output
- Binary RPM: `~/rpmbuild/RPMS/x86_64/drove-gateway-*.el9.x86_64.rpm`
- Source RPM: `~/rpmbuild/SRPMS/drove-gateway-*.el9.src.rpm`
- Build logs: `~/rpmbuild/BUILD/`

### Installation
```bash
sudo dnf install ~/rpmbuild/RPMS/x86_64/drove-gateway-*.el9.x86_64.rpm
sudo systemctl enable drove.gateway.service
sudo systemctl start drove.gateway.service
```

## Notes

- Both packages use the same binary and configuration files
- Systemd service files are standardized across both package formats
- Configuration migration is handled automatically during package upgrades using deb-systemd-helper (not systemctl)
- Packages can coexist with older `nixy` installations and provide automatic migration
docker run --rm -v $(pwd):/work -w /work debian:bookworm bash -c '
  apt-get update
  apt-get install -y build-essential debhelper-compat devscripts golang-go git
  dpkg-buildpackage -us -uc -b
'
```

### RPM Package in Container (UBI9)
```bash
docker run --rm -v $(pwd):/work -w /work quay.io/ubi9/ubi:latest bash -c '
  dnf install -y gcc golang systemd-devel rpm-build rpmdevtools redhat-rpm-config
  rpmdev-setuptree
  cp rpmbuild/SPECS/drove-gateway.spec ~/rpmbuild/SPECS/
  cd ..
  tar -czf ~/rpmbuild/SOURCES/drove-gateway-1.0.tar.gz drove-gateway/
  rpmbuild -ba ~/rpmbuild/SPECS/drove-gateway.spec
'
```

## CI/CD Workflows

### Debian Build Workflow
**Trigger**: Push to `main`, Tags matching `v*`
**Workflow**: `.github/workflows/debian-build.yml`

Builds Debian packages using official `dpkg-buildpackage` and uploads artifacts.

### RPM Build Workflow
**Trigger**: Push to `main`, Tags matching `v*`, Manual trigger
**Workflow**: `.github/workflows/rpm-build.yml`

Builds RPM packages in UBI9 container and uploads artifacts.

### Release Workflow
**Trigger**: Git tags matching `v*`
**Workflows**: `.github/workflows/release.yml` (Debian), `.github/workflows/rpm-release.yml` (RPM)

Creates GitHub releases with built packages and checksums using official tools:
- Debian: `dpkg-buildpackage`
- RPM: `rpmbuild`

### Creating a Release
```bash
# Tag the release
git tag -a v1.1 -m "Release version 1.1"
git push origin v1.1

# This will trigger:
# 1. Debian build and release
# 2. RPM build and release
# 3. Docker image build and push
```

## Version Management

Version is extracted from:
1. **Debian**: `debian/changelog`
2. **RPM**: `rpmbuild/SPECS/drove-gateway.spec`

When updating versions, update both files:
```bash
# Update debian/changelog (use dch command)
dch -i

# Update rpmbuild/SPECS/drove-gateway.spec
sed -i 's/^Version:.*/Version:        NEW_VERSION/' rpmbuild/SPECS/drove-gateway.spec
```

## Service Integration

The package automatically:
- Creates `/etc/nixy` configuration directory
- Installs systemd service: `drove.gateway.service`
- Sets up `nixy.service` as an alias for backward compatibility
- Handles migrations from older `nixy.service` installations

### Service Management
```bash
# Enable on boot
sudo systemctl enable drove.gateway.service

# Start/stop/restart
sudo systemctl start drove.gateway.service
sudo systemctl stop drove.gateway.service
sudo systemctl restart drove.gateway.service

# Check status
sudo systemctl status drove.gateway.service

# View logs
sudo journalctl -u drove.gateway.service -f
```

## Troubleshooting

### Build Failures

**Error**: `go: cannot find GOROOT`
- Set Go path: `export PATH=$PATH:/usr/local/go/bin`

**Error**: `systemd headers not found`
- Install systemd-devel: `sudo dnf install systemd-devel`

**Error**: `rpmbuild: command not found`
- Install rpm-build: `sudo dnf install rpm-build`

### Package Issues

**Check package contents**:
```bash
# Debian
dpkg -L drove-gateway

# RPM
rpm -ql drove-gateway
```

**Check dependencies**:
```bash
# Debian
dpkg -I drove-gateway_*.deb

# RPM
rpm -qi drove-gateway-*.rpm
```

## Notes

- Both packages use the same binary and configuration files
- Systemd service files are standardized across both package formats
- Configuration migration is handled automatically during package upgrades
- Packages can coexist with older `nixy` installations and provide automatic migration
