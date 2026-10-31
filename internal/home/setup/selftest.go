package setup

import (
	"context"
	"encoding/json"
	"fmt"
	"net/netip"
	"strings"
	"time"

	"github.com/Deikus-LXXVII/rubi/internal/home/agent"
)

// isolationProbe runs in the VM as the runner's user. It checks that /work/.rubi is writable; that the VM has no
// route and no interface besides loopback (it runs with --network none); that direct connections to a public address
// and to the gateways a Mac's container networks use fail with ENETUNREACH, and that name lookups fail; and that the
// proxy, reached on 127.0.0.1 through Rubi Home's pipe, lets a registry host through and refuses one not on the list.
const isolationProbe = `
const net = require("net"), dns = require("dns"), fs = require("fs");
const out = {};
try { fs.mkdirSync("/work/.rubi", {recursive: true}); fs.writeFileSync("/work/.rubi/selftest-probe", "ok"); fs.unlinkSync("/work/.rubi/selftest-probe"); out.write = "ok"; } catch (e) { out.write = e.code; }
out.uid = process.getuid();
const lines = f => { try { return fs.readFileSync(f, "utf8").split("\n").filter(Boolean); } catch (e) { return ["unreadable " + e.code]; } };
// every route that is not on lo: /proc/net/route has a header line, /proc/net/ipv6_route none (interface name last)
out.routes = lines("/proc/net/route").slice(1).filter(l => l.split(/\s+/)[0] !== "lo")
  .concat(lines("/proc/net/ipv6_route").filter(l => l.trim().split(/\s+/).pop() !== "lo"));
try { out.ifaces = fs.readdirSync("/sys/class/net").filter(n => n !== "lo"); } catch (e) { out.ifaces = ["unreadable " + e.code]; }
const tcp = (host, port) => new Promise(r => { const s = net.connect({host, port, timeout: 4000});
  s.on("connect", () => { s.destroy(); r("connected"); }); s.on("timeout", () => { s.destroy(); r("timeout"); }); s.on("error", e => r(e.code)); });
const connect = (host) => new Promise(r => { const u = new URL(process.env.HTTPS_PROXY);
  const s = net.connect({host: u.hostname, port: +u.port, timeout: 15000}, () => s.write("CONNECT " + host + ":443 HTTP/1.1\r\nHost: " + host +
    ":443\r\nProxy-Authorization: Basic " + Buffer.from(decodeURIComponent(u.username) + ":" + decodeURIComponent(u.password)).toString("base64") + "\r\n\r\n"));
  s.once("data", d => { s.destroy(); r(d.toString().split("\r\n")[0]); }); s.on("timeout", () => { s.destroy(); r("timeout"); }); s.on("error", e => r(e.code)); });
(async () => {
  out.dns = await new Promise(r => dns.lookup("registry.npmjs.org", (e, a) => r(e ? e.code : a)));
  out.direct_ip = await tcp("1.1.1.1", 443);
  out.direct_gateways = await Promise.all(["192.168.64.1", "192.168.128.1", "192.168.0.1", "10.0.0.1"].map(h => tcp(h, 53)));
  out.proxy_allowed = await connect("registry.npmjs.org");
  out.proxy_not_listed = await connect("example.org");
  console.log(JSON.stringify(out));
})();`

// selfTestPrompt is fixed: the self-test never runs a prompt of the user's.
const selfTestPrompt = "Use the Bash tool to run exactly `echo self-test`. If the tool call is denied, reply with the single word DENIED and nothing else."

// checkIsolation reads the probe's output and names the first check that failed ("" when all passed).
func checkIsolation(out []byte) string {
	var r map[string]any
	if err := json.Unmarshal([]byte(strings.TrimSpace(string(out))), &r); err != nil {
		return "the isolation probe gave no result"
	}
	switch {
	case r["write"] != "ok" || r["uid"] != float64(10001):
		return "the agent's user can't write the project's .rubi folder"
	case !emptyList(r["routes"]) || !emptyList(r["ifaces"]):
		return "the VM has a network route or interface besides loopback"
	case !dnsFailed(r["dns"]):
		return "the VM looks up names itself"
	case r["direct_ip"] != "ENETUNREACH" || !allUnreachable(r["direct_gateways"]):
		return "the VM connects to the Internet or to a gateway directly"
	case !strings.Contains(fmt.Sprint(r["proxy_allowed"]), " 200"):
		return "an allowed host isn't reachable through the proxy"
	case strings.Contains(fmt.Sprint(r["proxy_not_listed"]), " 200"):
		return "the proxy let through a host that isn't on the list"
	}
	return ""
}

// dnsFailed says whether the lookup gave an error code (and no IPv4 or IPv6 address).
func dnsFailed(v any) bool {
	s, ok := v.(string)
	if !ok || s == "" {
		return false
	}
	_, err := netip.ParseAddr(s)
	return err != nil
}

// emptyList says whether v is a JSON array with nothing in it (a missing field is not empty: the check didn't run).
func emptyList(v any) bool {
	l, ok := v.([]any)
	return ok && len(l) == 0
}

// allUnreachable: with no route, every direct connection fails at once with ENETUNREACH. Anything else (a timeout, a
// refusal, a connection) means an address answered, or a route exists.
func allUnreachable(v any) bool {
	l, ok := v.([]any)
	if !ok || len(l) == 0 {
		return false
	}
	for _, x := range l {
		if x != "ENETUNREACH" {
			return false
		}
	}
	return true
}

// checkTurn names what is wrong with the self-test's turn ("" when it is fine).
func checkTurn(r agent.TurnResult) string {
	denied := false
	for _, t := range r.Denied {
		denied = denied || t == "Bash"
	}
	switch {
	case r.Closed:
		return "the session closed before it answered"
	case r.IsError:
		return "the turn ended with an error"
	case !denied:
		return "the model didn't ask to run the command, so the denial wasn't tested"
	}
	return ""
}

func (s *Setup) runSelfTest(ctx context.Context, j *job, p agent.Project) outcome {
	log := j.log(s)
	finish := func(o outcome) outcome {
		if ctx.Err() != nil {
			return outcome{Todo, "Cancelled."}
		}
		s.mu.Lock()
		s.tests[p.Dir] = result{outcome: o, Progress: j.prog.snapshot()}
		s.mu.Unlock()
		return o
	}
	st, err := s.opt.Agents.BeginSelfTest(p.ID)
	if err != nil {
		return finish(outcome{Failed, safeText(err.Error())})
	}
	defer st.End()
	log("Starting the project's VM...")
	box, err := st.Sandbox(ctx)
	if err != nil {
		return finish(outcome{Failed, "Couldn't start the project's VM: " + safeText(err.Error())})
	}
	log("Checking the VM's isolation...")
	pctx, cancel := context.WithTimeout(ctx, 2*time.Minute)
	conn, err := box.Exec(pctx, []string{"node", "-e", isolationProbe}, nil)
	if err != nil {
		cancel()
		return finish(outcome{Failed, "Couldn't run the isolation check: " + safeText(err.Error())})
	}
	_ = conn.CloseWrite()
	out := readAll(pctx, conn, 1<<20)
	_ = conn.Close()
	cancel()
	if bad := checkIsolation(out); bad != "" {
		return finish(outcome{Failed, "Isolation check failed: " + bad + "."})
	}
	log("Isolation holds. Asking for one command that will be denied...")
	res, err := st.Turn(ctx, selfTestPrompt, 4*time.Minute)
	if err != nil {
		return finish(outcome{Failed, "The test turn failed: " + safeText(err.Error())})
	}
	if bad := checkTurn(res); bad != "" {
		return finish(outcome{Failed, "Turn check failed: " + bad + "."})
	}
	return finish(outcome{Done, "Isolation holds and a denied command stayed denied."})
}
