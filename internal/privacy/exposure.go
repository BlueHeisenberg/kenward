package privacy

import "fmt"

// Reach is how far the admin dashboard's listener goes.
//
// It is this package's own enumeration rather than config.Exposure, for the reason
// Mode is its own rather than config.Mode: what a topology protects must not be
// stated in terms of the shape of a configuration file. The caller maps.
type Reach int

const (
	// ReachOff is no listener at all, which is the state of every household that
	// has never opened the dashboard and the only state in which the statements in
	// this file are the whole truth on their own.
	ReachOff Reach = iota
	// ReachLoopback is a listener nothing off this machine can connect to.
	ReachLoopback
	// ReachTailnet is a listener on a tailnet or VPN interface: reachable from
	// elsewhere, over a network that has already authenticated and encrypted the
	// connection before kenward sees it.
	ReachTailnet
	// ReachLAN is a listener on the household's own network, where nothing has.
	ReachLAN
)

// DashboardNote is the paragraph the privacy statement gains once a port is listening.
//
// It exists because "whoever runs the machine" stops being the whole truth the moment
// the dashboard is on. The statements above are written about a machine somebody has to
// be sitting at; a listening socket adds a second way in, and the honest thing is to
// name it in the same place, in the same register, rather than leave a reader to find
// out from a port scan.
//
// It is a separate function rather than more text inside Statement because it is a fact
// about this node's configuration and Statement is a fact about the mode. Statement is
// golden-tested and must not vary with a household's settings; this varies with exactly
// one setting and is golden-tested against each of its values.
//
// tls says whether the connection is encrypted. It is only asked for the two reaches
// where it can be false and matter — a loopback socket has nothing in front of it to
// intercept, and a tailnet has already done it.
func DashboardNote(r Reach, addr string, tls bool) string {
	switch r {
	case ReachOff:
		return "The admin dashboard is off. No port is open, and the only way to change\n" +
			"anything here is at this machine, as whoever runs it."

	case ReachLoopback:
		return fmt.Sprintf(`The admin dashboard is listening on %s. That is this machine only:
nothing on your network, and nothing on the internet, can reach it. Whoever
can open a browser on this computer can log in with the admin password and,
from there, change where every member's conversations may go. That is the
same person the paragraphs above already say can read everything, so it adds
a door rather than a room.`, addr)

	case ReachTailnet:
		scheme := "http"
		if tls {
			scheme = "https"
		}
		return fmt.Sprintf(`The admin dashboard is listening on %s, reachable over your tailnet or
VPN and nowhere else. Anyone already on that network can reach the login
page, and the admin password is the only thing between them and every
setting in this household — including which conversations may leave the
house. The connection is %s; your tailnet has already authenticated and
encrypted it either way, which is why this is the recommended way in from
another machine.`, addr, scheme)

	case ReachLAN:
		if !tls {
			// kenward refuses to start in this state — validateDashboard requires
			// TLS under LAN exposure. Saying something true rather than something
			// reassuring is this package's whole job, so it says it anyway.
			return fmt.Sprintf(`The admin dashboard is listening on %s over plain HTTP, on your own
network. Everyone on your wifi can reach the login page, and the admin
password crosses the network in the clear. This is not a configuration
kenward will start with, and it should not be one you run.`, addr)
		}
		return fmt.Sprintf(`The admin dashboard is listening on %s over HTTPS, on your own network.
Everyone on your wifi can reach the login page — a guest, a smart TV,
anything that has ever had the password — and the admin password is the only
thing between them and every setting in this household, including which
conversations may leave the house. The certificate is one this machine
generated and signed itself, so your browser will warn you the first time;
check the fingerprint the dashboard showed you against the one the browser
shows, once, and you will know you are talking to this machine and not to
something that answered instead of it.`, addr)

	default:
		return ""
	}
}
