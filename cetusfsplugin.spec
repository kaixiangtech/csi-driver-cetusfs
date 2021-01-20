Name:   cetusfsplugin
Version:        %{VERSION}_%{CUR_BR}_%{GIT_COMMIT}
Release:        %{?GIT_VER}
Summary:        Kaixiangtech CetusFS k8s csi plugin

Group:  System Environment/Base
License:        GPL


%description
Kaixiangtech CetusFS k8s csi plugin



%install
make install DESTDIR=%{buildroot}


%files
/usr/sbin/cetusfsplugin
/lib/systemd/system/cetusfsplugin.service
/var/log/cetusfsplugin/
/var/lib/kubelet/plugins/csi-cetusfs/
/csi-cetusfs-data-dir/
/usr/share/doc/cetusfsplugin/kubernetes/cetusfs/csi-cetusfs-attacher.yaml
/usr/share/doc/cetusfsplugin/kubernetes/cetusfs/csi-cetusfs-driverinfo.yaml
/usr/share/doc/cetusfsplugin/kubernetes/cetusfs/csi-cetusfs-plugin.yaml
/usr/share/doc/cetusfsplugin/kubernetes/cetusfs/csi-cetusfs-provisioner.yaml
/usr/share/doc/cetusfsplugin/kubernetes/cetusfs/csi-cetusfs-resizer.yaml
/usr/share/doc/cetusfsplugin/kubernetes/cetusfs/csi-cetusfs-testing.yaml
/usr/share/doc/cetusfsplugin/kubernetes/cetusfs/csi-cetusfs-storageclass.yaml
/usr/share/doc/cetusfsplugin/kubernetes/cetusfs/csi-cetusfs-attacher-rbac.yaml
/usr/share/doc/cetusfsplugin/kubernetes/cetusfs/csi-cetusfs-provisioner-rbac.yaml
/usr/share/doc/cetusfsplugin/kubernetes/cetusfs/csi-cetusfs-resizer-rbac.yaml
%doc


%post
systemctl daemon-reload


%changelog

