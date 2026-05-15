# Packaging Structure

This document describes the packaging structure for drove-gateway supporting both Debian and RPM formats.

## Directory Structure

```
drove-gateway/
├── debian/                    # Debian package files
│   ├── changelog             # Version history
│   ├── compat                # Debhelper compatibility level (12)
│   ├── control               # Package metadata and dependencies
│   ├── install               # File installation manifest
│   ├── postinst              # Post-installation script
│   ├── postrm                # Post-removal script
│   ├── preinst               # Pre-installation script
│   └── rules                 # Debian build recipe
│
├── rpmbuild/                 # RPM package files
│   ├── SPECS/
│   │   └── drove-gateway.spec # RPM specification file
│   └── README.md              # RPM build documentation
│
├── scripts/                  # Build helper scripts
│   ├── debbuild.sh          # Helper script for local Debian builds
│   ├── rpmbuild.sh          # Helper script for local RPM builds
│   └── ...
│
├── .github/workflows/        # CI/CD workflows
│   ├── debian-build.yml     # Debian package build (matrix, gated)
│   ├── debian-release.yml   # Debian release (GitHub, tags)
│   ├── rpm-build.yml        # RPM package build (EL9/EL10, gated)
│   ├── rpm-release.yml      # RPM release (GitHub, tags)
│   ├── docker-build.yml     # Docker image build
│   ├── lint.yml             # Code quality checks
│   └── security.yml         # Security scanning
│
└── BUILD.md                 # Comprehensive build documentation
```

## Package Information

### Common Details
- **Name**: drove-gateway
- **Description**: Nginx/HAProxy configuration management daemon for Drove
- **Homepage**: https://github.com/phonepe/drove-gateway
- **License**: Apache-2.0 (specified in spec file)
- **Binary**: `/usr/bin/nixy`
- **Configuration**: `/etc/nixy/`
- **Service**: `drove.gateway.service` (with `nixy.service` alias)

### Debian Package
- **Format**: .deb (Debian binary)
- **Build System**: debhelper (via official `dpkg-buildpackage`)
- **Compatibility**: Debian 12+, Debian 13, Ubuntu 20.04/22.04/24.04
- **Build File**: `debian/rules`
- **Control File**: `debian/control`
- **Go Version Required**: >= 1.23 (from longsleep/golang-backports PPA for Ubuntu 20.04)
- **Official Tool**: `dpkg-buildpackage` (standard Debian packaging tool)

### RPM Package
- **Format**: .rpm (Red Hat Package Manager)
- **Build System**: rpmbuild
- **Compatibility**: RHEL 9/10, CentOS 9, AlmaLinux 9, Rocky Linux 9, Oracle Linux 9
- **Spec File**: `rpmbuild/SPECS/drove-gateway.spec`
- **Go Version Required**: >= 1.23
- **Container Image**: quay.io/ubi9/ubi (UBI9/UBI10 for compatibility)

## Installation Files

Both package formats install the same files to consistent system locations:

```
/usr/bin/nixy                        # Main executable binary
/etc/nixy/                           # Configuration directory
/etc/nixy/nixy.toml.example          # Example configuration
/etc/nixy/*.tmpl                     # Nginx templates
/etc/nixy/*.conf                     # Additional configs
/lib/systemd/system/drove.gateway.service  # Systemd unit
/usr/share/doc/drove-gateway/        # Documentation
```

## Build Process Comparison

### Debian Build (`dpkg-buildpackage` - Official Tool)
1. Source verification and setup
2. Build dependencies checked via `Build-Depends` in control file
3. Execution of `debian/rules` targets (via debhelper)
4. Binary compilation (via `override_dh_auto_build` in rules)
5. File installation to temporary package root (via `override_dh_auto_install`)
6. Package metadata generation from control files
7. Creation of `.deb` binary package
8. Optional: Creation of `.dsc` and `.tar.gz` for source package
9. Linting with `lintian` for Debian compliance

### RPM Build (`rpmbuild`)
1. Source tarball extraction
2. Dependency installation via `BuildRequires`
3. Build in `%build` section
4. Install in `%install` section with buildroot
5. Create `.rpm` from `%files` manifest
6. Include pre/post scripts automatically
7. Generate `.src.rpm` (source RPM)

## Service Integration

### Systemd Unit File
Located in: `support/drove.gateway.service`

Features:
- Primary name: `drove.gateway.service`
- Alias: `nixy.service` (for backward compatibility)
- Starts after: `network.target`, `nss-lookup.target`
- Wants: `nginx.service` or `haproxy.service` (optional)
- Restart policy: always with 1 second delay

### Migration from `nixy.service`
When upgrading from older packages:
1. `preinst` detects if `nixy.service` was enabled (using deb-systemd-helper)
2. Saves state in `/var/lib/drove-gateway/.nixy-was-enabled`
3. `postinst` migrates enablement to `drove.gateway.service` (using deb-systemd-helper)
4. Service starts automatically with new name

## Version Management

### Debian Version
- File: `debian/changelog`
- Format: `packagename (VERSION-RELEASE) distribution; urgency=LEVEL`
- Example: `drove-gateway (1.0-1) unstable; urgency=medium`
- Tool: Use `dch` command to update

### RPM Version
- File: `rpmbuild/SPECS/drove-gateway.spec`
- Fields:
  - `Version:` - Main version number (1.0)
  - `Release:` - Release number with dist tag (1%{?dist} expands to el9)
  - `%changelog` - Change history
- Example: `drove-gateway-1.0-1.el9.x86_64.rpm`

## CI/CD Workflows

### Build Workflows
- Trigger on: Push to main/develop, Pull requests
- Actions: Build, test, lint, upload artifacts

### Release Workflows
- Trigger on: Git tags (v*)
- Actions: Build packages, generate checksums, create GitHub release

### Container Builds
- Docker: Build and push to ghcr.io
- Scheduled on: Tags, main branch

### Security Scanning
- Trigger: Weekly schedule + on push
- Tools: Trivy, gosec, nancy

## Build Commands

### Local Debian Build (Official Debian Way)
```bash
# Recommended: Use official dpkg-buildpackage directly
dpkg-buildpackage -us -uc -b          # Binary only (faster)
dpkg-buildpackage -us -uc             # Source and binary
```

### Local RPM Build
```bash
# With helper script (if present):
./scripts/rpmbuild.sh [VERSION]
# Or manual:
rpmdev-setuptree
rpmbuild -ba rpmbuild/SPECS/drove-gateway.spec
```

### Docker Builds

**Debian in Container (Official Method)**:
```bash
# Example for Debian 13 (trixie):
docker run --rm -v $(pwd):/work -w /work debian:trixie \
  bash -c 'apt-get update && apt-get install -y build-essential debhelper-compat devscripts golang-go git && dpkg-buildpackage -us -uc -b'
```

**RPM in Container**:
```bash
docker run --rm -v $(pwd):/work -w /work quay.io/ubi9/ubi:latest \
  bash -c 'dnf install -y rpm-build golang && rpmbuild -ba rpmbuild/SPECS/drove-gateway.spec'
```

## File Permissions

### Configuration Files
- `nixy.toml`: 644 (user readable)
- `*.tmpl`: 644 (user readable)
- `/etc/nixy/`: 755 (world readable)

### Binary
- `/usr/bin/nixy`: 755 (world executable)

### Service File
- `drove.gateway.service`: 644 (standard)

## Dependency Resolution

### Build Dependencies
Both formats require:
- Go compiler (>= 1.23)
- Git (for version info)
- Standard build tools (gcc, make)
- Systemd development headers

### Runtime Dependencies
- Systemd (service management)
- Nginx OR HAProxy (proxy software)

## Notes

- Both Debian and RPM packages produce identical binaries and configuration
- Version strings include commit hash and build date
- Packages handle service migration automatically (using deb-systemd-helper, not systemctl)
- Configuration is preserved during package upgrades
- Uninstall removes config only on `purge` (Debian) or uninstall (RPM)
