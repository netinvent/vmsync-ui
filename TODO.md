We miss a howto.

Eg insecure=true if behind a https proxy, else how do we generate tls certs
dynamicuser=On isn't good, we should create vmsync-ui user
Or analyze those logs: 

sept. 16 19:57:23 hyper01p.domain.local systemd[1]: Started vmsync-ui.service - vmsync control-plane UI.
sept. 16 19:57:23 hyper01p.domain.local (msync-ui)[12965]: vmsync-ui.service: Found pre-existing public StateDirectory= directory /var/lib/vmsync-ui, migrating to /var/lib/private/vmsync-ui.
sept. 16 19:57:23 hyper01p.domain.local (msync-ui)[12965]: vmsync-ui.service: Apparently, service previously had DynamicUser= turned off, and has now turned it on.
sept. 16 19:57:23 hyper01p.domain.local (msync-ui)[12965]: vmsync-ui.service: Failed to set up special execution directory in /var/lib: Permission denied
sept. 16 19:57:23 hyper01p.domain.local (msync-ui)[12965]: vmsync-ui.service: Failed at step STATE_DIRECTORY spawning /usr/local/bin/vmsync-ui: Permission denied

enrolling an agent ?should be explained:
eg create an enroll token in UI, then enroll agent with it


Simplified (non-CA) setup with just one shared certificate between agents and ui

CERT_DIR=/etc/vmsync-ui
COMMON_NAME=dr.domainistrateur.com
[ ! -d "${CERT_DIR}/tls" ] && mkdir "${CERT_DIR}/tls"
openssl req -nodes -new -x509 -days 7300 -newkey rsa:4096 -keyout ${CERT_DIR}/tls/${COMMON_NAME// /_}.key -subj "/C=FR/O=domain/CN=${COMMON_NAME}/OU=RD/L=domainVille/ST=IleVolcan/emailAddress=contact@domainistrateur.com"  -addext "subjectAltName=DNS:${COMMON_NAME}"  -out ${CERT_DIR}/tls/${COMMON_NAME// /_}.crt

vmsync-ui should show version in footer

vmsync-ui enroll process shows:
"vmsync-agent -ui <this UI> --enrol-token e86d39ac845508daae2f08d479445c3dd950a389da8a84b0945793a0b93c31 --once"
should show
"vmsync-agent -enrol-token-file /tmp/enrol -once -monitor"


vmsync-agent should use -enrol-token and not -enrol-token-file

