// The git credential socket inside the VM. git-credential-rubi (runner/container) connects here:
// first line the operation (get|store|erase), then git's key=value lines up to the blank line;
// the reply is git's text; the runner closes the connection. The runner relays it to Rubi Home as a
// git.credential request and never keeps, logs or interprets the credential.
import fs from "node:fs";
import net from "node:net";

export const MAX_CRED_REQUEST_BYTES = 8192;
const OPS = new Set(["get", "store", "erase"]);

export interface CredServer {
  close(): void;
}

export function serveCredentials(
  path: string,
  handle: (op: string, input: string) => Promise<string>,
  log: (m: string) => void,
): Promise<CredServer> {
  try {
    fs.unlinkSync(path); // a stale socket of an earlier runner
  } catch {
    // none
  }
  const server = net.createServer((c) => {
    let buf = "";
    let done = false;
    const idle = setTimeout(() => c.destroy(), 10_000); // the helper sends everything at once
    const finish = () => {
      if (done) return;
      done = true;
      clearTimeout(idle);
      const nl = buf.indexOf("\n");
      const op = (nl < 0 ? buf : buf.slice(0, nl)).trim();
      if (!OPS.has(op)) {
        c.end();
        return;
      }
      const input = nl < 0 ? "" : buf.slice(nl + 1);
      handle(op, input).then(
        (out) => c.end(out),
        (e) => {
          log(`git.credential failed: ${e instanceof Error ? e.message : String(e)}`);
          c.end(); // no answer: git fails without prompting
        },
      );
    };
    c.setEncoding("utf8");
    c.on("data", (d: string) => {
      if (done) return;
      buf += d;
      if (buf.length > MAX_CRED_REQUEST_BYTES) {
        done = true;
        c.destroy();
        return;
      }
      if (buf.includes("\n\n")) finish(); // git ends its text with a blank line
    });
    c.on("end", finish);
    c.on("error", () => {});
  });
  return new Promise((resolve, reject) => {
    server.once("error", reject);
    const old = process.umask(0o177); // created 0600
    server.listen(path, () => {
      process.umask(old);
      try {
        fs.chmodSync(path, 0o600);
      } catch {
        // best effort; umask already did it
      }
      resolve({
        close: () => {
          server.close();
          try {
            fs.unlinkSync(path);
          } catch {
            // gone
          }
        },
      });
    });
  });
}
