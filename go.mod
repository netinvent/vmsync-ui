// vmsync-ui deliberately depends on nothing outside the standard library.
//
// It talks to vmsync agents over JSON/HTTPS and never touches libvirt,
// libnbd or any of vmsync's own Go packages -- which is why it lives in its
// own module rather than inside the vmsync repository. Sharing that module
// would drag the whole cgo/libvirt/libnbd build requirement into a program
// that has no use for it.
module vmsync-ui

go 1.24
