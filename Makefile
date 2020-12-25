VERSION=21.01.01.1
GIT_VER=$(shell git rev-list HEAD| wc -l)
GIT_COMMIT=$(shell git rev-parse --short HEAD)

all:
	go build -a -ldflags ' -X main.version=${VERSION}_${GIT_COMMIT}_${GIT_VER}  -extldflags "-static"' -o ./bin/cetusfsplugin ./cmd/cetusfsplugin
clean:
	rm -fr ./bin/
install:
	mkdir -p $(DESTDIR)/var/log/cetusfsplugin/
	mkdir -p ${DESTDIR}/var/lib/kubelet/plugins/csi-cetusfs/
	mkdir -p ${DESTDIR}/usr/sbin/
	mkdir -p ${DESTDIR}/lib/systemd/system/
	mkdir -p ${DESTDIR}/usr/share/doc/cetusfsplugin/
	mkdir -p ${DESTDIR}/usr/share/doc/cetusfsplugin/kubernetes/cetusfs/
	install -m 755 bin/cetusfsplugin $(DESTDIR)/usr/sbin/
	install -m 644 systemctl/cetusfsplugin.service $(DESTDIR)/lib/systemd/system/
	install -m 644 deploy/kubernetes/cetusfs/csi-cetusfs-attacher.yaml ${DESTDIR}/usr/share/doc/cetusfsplugin/kubernetes/cetusfs/csi-cetusfs-attacher.yaml
	install -m 644 deploy/kubernetes/cetusfs/csi-cetusfs-driverinfo.yaml ${DESTDIR}/usr/share/doc/cetusfsplugin/kubernetes/cetusfs/csi-cetusfs-driverinfo.yaml
	install -m 644 deploy/kubernetes/cetusfs/csi-cetusfs-plugin.yaml ${DESTDIR}/usr/share/doc/cetusfsplugin/kubernetes/cetusfs/csi-cetusfs-plugin.yaml
	install -m 644 deploy/kubernetes/cetusfs/csi-cetusfs-provisioner.yaml ${DESTDIR}/usr/share/doc/cetusfsplugin/kubernetes/cetusfs/csi-cetusfs-provisioner.yaml
	install -m 644 deploy/kubernetes/cetusfs/csi-cetusfs-resizer.yaml ${DESTDIR}/usr/share/doc/cetusfsplugin/kubernetes/cetusfs/csi-cetusfs-resizer.yaml
	install -m 644 deploy/kubernetes/cetusfs/csi-cetusfs-testing.yaml ${DESTDIR}/usr/share/doc/cetusfsplugin/kubernetes/cetusfs/csi-cetusfs-testing.yaml
	install -m 644 deploy/kubernetes/cetusfs/csi-cetusfs-storageclass.yaml ${DESTDIR}/usr/share/doc/cetusfsplugin/kubernetes/cetusfs/csi-cetusfs-storageclass.yaml
	$(shell systemctl daemon-reload)
uninstall:
	rm -f /usr/sbin/cetusfsplugin
	rm -f /lib/systemd/system/cetusfsplugin.service
	$(shell systemctl daemon-reload)


PHONY: all clean install uninstall

