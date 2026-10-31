package setup

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"
	"time"

	"github.com/Deikus-LXXVII/rubi/internal/home/agent"
)

// isolationProbe runs in the VM as the runner's user. It checks that /work/.rubi is writable, that direct
// connections and name lookups fail (the first address is a public one, the second the default NAT gateway of
// Apple's container tool), and that the proxy lets an Anthropic host through and refuses one not on the list.
const isolationProbe = `
const net = require("net"), dns = require("dns"), fs = require("fs");
const out = {};
try { fs.mkdirSync("/work/.rubi", {recursive: true}); fs.writeFileSync("/work/.rubi/selftest-probe", "ok"); fs.unlinkSync("/work/.rubi/selftest-probe"); out.write = "ok"; } catch (e) { out.write = e.code; }
out.uid = process.getuid();
const tcp = (host, port) => new Promise(r => { const s = net.connect({host, port, timeout: 4000});
  s.on("connect", () => { s.destroy(); r("connected"); }); s.on("timeout", () => { s.destroy(); r("timeout"); }); s.on("error", e => r(e.code)); });
const connect = (host) => new Promise(r => { const u = new URL(process.env.HTTPS_PROXY);
  const s = net.connect({host: u.hostname, port: +u.port, timeout: 15000}, () => s.write("CONNECT " + host + ":443 HTTP/1.1\r\nHost: " + host +
    ":443\r\nProxy-Authorization: Basic " + Buffer.from(decodeURIComponent(u.username) + ":" + decodeURIComponent(u.password)).toString("base64") + "\r\n\r\n"));
  s.once("data", d => { s.destroy(); r(d.toString().split("\r\n")[0]); }); s.on("timeout", () => { s.destroy(); r("timeout"); }); s.on("error", e => r(e.code)); });
(async () => {
  out.dns = await new Promise(r => dns.lookup("api.anthropic.com", (e, a) => r(e ? e.code : a)));
  out.direct_ip = await tcp("1.1.1.1", 443);
  out.direct_default_gw = await tcp("192.168.64.1", 53);
  out.proxy_allowed = await connect("api.anthropic.com");
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
	case r["dns"] == nil || strings.Contains(fmt.Sprint(r["dns"]), "."):
		return "the VM looks up names itself"
	case r["direct_ip"] == "connected" || r["direct_default_gw"] == "connected":
		return "the VM connects to the Internet directly"
	case !strings.Contains(fmt.Sprint(r["proxy_allowed"]), " 200"):
		return "an allowed host isn't reachable through the proxy"
	case strings.Contains(fmt.Sprint(r["proxy_not_listed"]), " 200"):
		return "the proxy let through a host that isn't on the list"
	}
	return ""
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
