Name:           drove-gateway
Version:        1.0
Release:        1%{?dist}
Summary:        Drove gateway daemon for Nginx/HAProxy configuration management
License:        Apache-2.0
URL:            https://github.com/phonepe/drove-gateway
Source0:        %{name}-%{version}.tar.gz

BuildRequires:  gcc
BuildRequires:  golang >= 1.23
BuildRequires:  git
BuildRequires:  systemd-rpm-macros

Requires:       systemd
Requires:       (nginx or haproxy)
Suggests:       nginx-plus

%description
Drove Gateway (Nixy) is a daemon that automatically configures Nginx or HAProxy
for services deployed on the Drove container orchestrator. It monitors the Drove
event stream and dynamically updates proxy configurations in real-time.

Features:
 - Real-time configuration updates via Drove's event stream
 - Support for Nginx (OSS and Plus) with full reloads and dynamic upstream updates
 - Support for HAProxy with configuration reloads and runtime API updates
 - Automatic service discovery and health status tracking
 - Multi-controller Drove cluster support with automatic leader detection
 - Header-based routing support (HAProxy)
 - Single statically-linked binary with no runtime dependencies except Nginx/HAProxy

%prep
# Tarball contains a 'drove-gateway' top-level directory
%setup -q -n drove-gateway

%build
# go.mod lives in src/, cmd/ lives at the repo root
cd src
go mod tidy
go build -v -ldflags="-X main.version=%{version} -X main.date=$(date '+%%Y-%%m-%%d %%H:%%M:%%S') -X main.commit=$(git rev-parse --short HEAD 2>/dev/null || echo unknown)" -o ../nixy ../cmd/nixy

%install
# Create directories
mkdir -p %{buildroot}%{_bindir}
mkdir -p %{buildroot}%{_sysconfdir}/nixy
mkdir -p %{buildroot}%{_unitdir}
mkdir -p %{buildroot}%{_docdir}/%{name}
mkdir -p %{buildroot}%{_docdir}/%{name}/examples

# Install binary
install -m 0755 nixy %{buildroot}%{_bindir}/nixy

# Install systemd service file
install -m 0644 support/drove.gateway.service %{buildroot}%{_unitdir}/

# Install configuration files
install -m 0644 src/nixy.toml %{buildroot}%{_sysconfdir}/nixy/nixy.toml.example

# Install templates
install -m 0644 src/*.tmpl %{buildroot}%{_sysconfdir}/nixy/ 2>/dev/null || true
install -m 0644 src/*.conf %{buildroot}%{_sysconfdir}/nixy/ 2>/dev/null || true

# Install documentation and examples
install -m 0644 README.md %{buildroot}%{_docdir}/%{name}/
install -m 0644 examples/*.tmpl %{buildroot}%{_docdir}/%{name}/examples/ 2>/dev/null || true
install -m 0644 examples/nixy.toml %{buildroot}%{_docdir}/%{name}/examples/nixy.toml.example 2>/dev/null || true

%pre
# Create nixy user if it doesn't exist (optional: currently runs as root)
# getent passwd nixy > /dev/null || useradd -r -s /bin/false -d /var/lib/nixy nixy

%post
# Create configuration directory if it doesn't exist
mkdir -p %{_sysconfdir}/nixy
chmod 755 %{_sysconfdir}/nixy

# Install default configuration if it doesn't exist
if [ ! -f %{_sysconfdir}/nixy/nixy.toml ]; then
    if [ -f %{_sysconfdir}/nixy/nixy.toml.example ]; then
        cp %{_sysconfdir}/nixy/nixy.toml.example %{_sysconfdir}/nixy/nixy.toml
        chmod 644 %{_sysconfdir}/nixy/nixy.toml
    fi
fi

# Ensure templates are readable
if [ -d %{_sysconfdir}/nixy ]; then
    chmod 644 %{_sysconfdir}/nixy/*.tmpl 2>/dev/null || true
fi

# Create state directory for migrations
mkdir -p /var/lib/drove-gateway

# Migration from older nixy.service to drove.gateway.service
if [ -f /var/lib/drove-gateway/.nixy-was-enabled ]; then
    echo "Migrating from nixy.service to drove.gateway.service..."
    
    # Disable the old nixy.service if present
    if systemctl list-unit-files | grep -q "^nixy.service"; then
        systemctl disable nixy.service 2>/dev/null || true
    fi
    
    # Enable and start the new drove.gateway.service
    systemctl enable drove.gateway.service
    systemctl daemon-reload
    systemctl start drove.gateway.service
    
    # Clean up migration marker
    rm -f /var/lib/drove-gateway/.nixy-was-enabled
    rmdir /var/lib/drove-gateway 2>/dev/null || true
    
    echo "Migration completed: Service is now running as drove.gateway.service"
fi

%systemd_post drove.gateway.service

%preun
# Stop the service before uninstall (except on upgrade)
if [ $1 -eq 0 ]; then
    # This is an uninstall (not an upgrade)
    if systemctl is-active --quiet drove.gateway.service 2>/dev/null; then
        systemctl stop drove.gateway.service || true
    fi
fi

%systemd_preun drove.gateway.service

%postun
%systemd_postun_with_restart drove.gateway.service

# Clean up configuration on uninstall (not upgrade)
if [ $1 -eq 0 ]; then
    if [ -d %{_sysconfdir}/nixy ]; then
        rm -rf %{_sysconfdir}/nixy
    fi
fi

%files
%doc %{_docdir}/%{name}/README.md
%docdir %{_docdir}/%{name}/examples

%{_bindir}/nixy
%{_unitdir}/drove.gateway.service
%dir %{_sysconfdir}/nixy
%config(noreplace) %{_sysconfdir}/nixy/nixy.toml.example
%config(noreplace) %{_sysconfdir}/nixy/*.tmpl
%config(noreplace) %{_sysconfdir}/nixy/*.conf

%changelog
* Thu Feb 26 2026 vishnu naini <vishnu.naini@phonepe.com> - 1.0-1
- Initial release with deb/rpm packaging
- Real-time Drove event stream integration
- Support for Nginx and HAProxy configuration management
- Automatic service discovery and health tracking
