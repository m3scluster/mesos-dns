Name:           mesos-dns
Version:        %{VERSION}
Release:        0%{?dist}
Summary:        DNS Based service discovery for mesos
Group:          System Environment/Daemons
License:        Apache-2.0 License
URL:            https://www.aventer.biz

Obsoletes:	    mesos-dns < %{version}-%{release}
Provides:	      mesos-dns = %{version}-%{release}
BuildRoot:      %{_tmppath}/%{name}-%{version}-%{release}-root-%(%{__id_u} -n)

%description
DNS Based service discovery for mesos

%install
rm -rf $RPM_BUILD_ROOT
mkdir -p $RPM_BUILD_ROOT/usr/bin/
mkdir -p $RPM_BUILD_ROOT/etc/mesos-dns/
install -m 0755 %{_builddir}/mesos-dns $RPM_BUILD_ROOT/usr/bin/mesos-dns

%files
/usr/bin/mesos-dns
/etc/mesos-dns/
