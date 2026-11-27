# SPDX-FileCopyrightText: 2026 Defilippo srls info@defilippo.org
# SPDX-License-Identifier: AGPL-3.0-or-later

%global debug_package %{nil}
%global _build_id_links none
Name:           ewsgo
Version:        %{package_version}
Release:        1%{?dist}
Summary:        EWS and MAPI over HTTP gateway for mail servers
License:        AGPL-3.0-or-later
Requires:       ca-certificates
Requires(pre):  shadow-utils

%description
EWSGo connects compatible clients to a configurable mail backend.
The first backend uses the native Zimbra SOAP and REST interfaces.

%install
install -Dpm755 %{input_dir}/ewsgo %{buildroot}%{_bindir}/ewsgo
install -Dpm644 %{packaging_dir}/ewsgo.yaml %{buildroot}%{_sysconfdir}/ewsgo/ewsgo.yaml
install -Dpm644 %{packaging_dir}/ewsgo.service %{buildroot}%{_prefix}/lib/systemd/system/ewsgo.service
install -dm700 %{buildroot}%{_localstatedir}/lib/ewsgo
install -Dpm644 %{input_dir}/README.md %{buildroot}%{_docdir}/%{name}/README.md
install -pm644 %{input_dir}/README.it.md %{buildroot}%{_docdir}/%{name}/
install -Dpm644 %{input_dir}/LICENSE %{buildroot}%{_licensedir}/%{name}/LICENSE
ln -s ../../licenses/%{name}/LICENSE %{buildroot}%{_docdir}/%{name}/LICENSE
cp -rf %{input_dir}/assets %{input_dir}/docs %{buildroot}%{_docdir}/%{name}/
cp -r %{input_dir}/licenses %{buildroot}%{_licensedir}/%{name}/third-party

%pre
set -e
getent group ewsgo >/dev/null || groupadd --system ewsgo
getent passwd ewsgo >/dev/null || useradd --system --gid ewsgo --home-dir /var/lib/ewsgo --no-create-home --shell /sbin/nologin ewsgo

%preun
if [ "$1" -eq 0 ] && [ -d /run/systemd/system ]; then
    systemctl stop ewsgo.service || :
    systemctl --no-reload disable ewsgo.service || :
fi

%posttrans
if [ -d /run/systemd/system ]; then
    systemctl daemon-reload || :
fi

%postun
if [ "$1" -eq 0 ] && [ -d /run/systemd/system ]; then
    systemctl daemon-reload || :
fi

%files
%{_bindir}/ewsgo
%dir %{_sysconfdir}/ewsgo
%config(noreplace) %{_sysconfdir}/ewsgo/ewsgo.yaml
%{_prefix}/lib/systemd/system/ewsgo.service
%attr(0700,ewsgo,ewsgo) %dir %{_localstatedir}/lib/ewsgo
%doc %{_docdir}/%{name}
%license %{_licensedir}/%{name}
