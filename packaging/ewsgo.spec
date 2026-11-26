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

%description
EWSGo connects compatible clients to a configurable mail backend.
The first backend uses the native Zimbra SOAP and REST interfaces.

%install
install -Dpm755 %{input_dir}/ewsgo %{buildroot}%{_bindir}/ewsgo
install -Dpm644 %{input_dir}/README.md %{buildroot}%{_docdir}/%{name}/README.md
install -pm644 %{input_dir}/README.it.md %{buildroot}%{_docdir}/%{name}/
install -Dpm644 %{input_dir}/LICENSE %{buildroot}%{_licensedir}/%{name}/LICENSE
ln -s ../../licenses/%{name}/LICENSE %{buildroot}%{_docdir}/%{name}/LICENSE
cp -rf %{input_dir}/assets %{input_dir}/docs %{buildroot}%{_docdir}/%{name}/
cp -r %{input_dir}/licenses %{buildroot}%{_licensedir}/%{name}/third-party

%files
%{_bindir}/ewsgo
%doc %{_docdir}/%{name}
%license %{_licensedir}/%{name}
