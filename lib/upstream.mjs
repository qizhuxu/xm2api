/**
 * Shared upstream forwarding core.
 *
 * Both route servers (path2 = SSO session, path3 = API key) are thin wrappers
 * around this: they supply a route table plus a credential injector, and get a
 * ready http.Server with consistent logging, CORS, SSE passthrough and errors.
 *
 * Verified behaviours kept from the original proxy.mjs:
 *   - SSE is streamed chunk-by-chunk (no buffering) with a bounded log copy
 *   - non-SSE responses are buffered and logged in full
 *   - credentials are only attached when the caller did not send their own
 */
import http from "node:http";
import https from "node:https";
import fs from "node:fs";
import path from "node:path";

export const UPSTREAM_TIMEOUT_MS = 120_000;
const SSE_CAPTURE_CAP = 20_000;

/** First matching route wins, so keep specific entries above catch-alls. */
export function pickRoute(routes, pathname) {
  for (const r of routes) {
    if (r.match(pathname)) return r;
  }
  return null;
}

export function redactHeaders(headers) {
  const out = {};
  for (const [k, v] of Object.entries(headers || {})) {
    const lk = k.toLowerCase();
    if (["authorization", "cookie", "x-api-key", "set-cookie"].includes(lk)) {
      const s = String(v);
      out[k] = s.length > 12 ? `${s.slice(0, 8)}…(${s.length})` : "***";
    } else {
      out[k] = v;
    }
  }
  return out;
}

export function sendJson(res, status, obj) {
  const body = JSON.stringify(obj, null, 2);
  res.writeHead(status, { "content-type": "application/json" });
  res.end(body);
}

/**
 * @param {object} opts
 * @param {string} opts.name            human name for logs / meta
 * @param {number} opts.port
 * @param {string} [opts.host]
 * @param {Array}  opts.routes          [{ match(pathname), target, rewrite(pathname) }]
 * @param {Function} [opts.inject]      (headers, target) => void  — attach credentials
 * @param {string} opts.logDir
 * @param {string} opts.logPrefix       e.g. "path2" → logs/path2-capture-<date>.jsonl
 * @param {Function} [opts.meta]        () => object, served at /, /health, /__meta
 * @param {Array}  [opts.local]         [{ method, path, handler(req,res) }] — served before proxying
 */
export function createRouteServer(opts) {
  const { name, port, host = "127.0.0.1", routes, inject, logDir, logPrefix, meta, local = [] } = opts;
  fs.mkdirSync(logDir, { recursive: true });

  function logEntry(entry) {
    const file = path.join(logDir, `${logPrefix}-capture-${new Date().toISOString().slice(0, 10)}.jsonl`);
    try {
      fs.appendFileSync(file, JSON.stringify(entry) + "\n");
    } catch {}
    console.log(JSON.stringify(entry, null, 2));
  }

  function forward(clientReq, clientRes, bodyBuf) {
    const u = new URL(clientReq.url, `http://${clientReq.headers.host || "127.0.0.1"}`);
    const route = pickRoute(routes, u.pathname);
    if (!route) {
      sendJson(clientRes, 404, { error: { message: `no route for ${clientReq.method} ${u.pathname}` } });
      return;
    }
    const upstreamPath = route.rewrite(u.pathname) + u.search;
    const target = new URL(upstreamPath, route.target);
    const isHttps = target.protocol === "https:";
    const lib = isHttps ? https : http;

    const headers = { ...clientReq.headers };
    delete headers.host;
    delete headers["content-length"];
    if (bodyBuf && bodyBuf.length) headers["content-length"] = String(bodyBuf.length);
    if (!headers["user-agent"]) headers["user-agent"] = `${logPrefix}-route-server/1.0`;

    // Snapshot before injection so the log can show what WE attached (masked).
    const before = { authorization: headers.authorization, cookie: headers.cookie };
    if (inject) inject(headers, target);
    const injected = {};
    if (!before.authorization && headers.authorization) {
      injected.authorization = redactHeaders({ authorization: headers.authorization }).authorization;
    }
    if (!before.cookie && headers.cookie) {
      injected.cookie = redactHeaders({ cookie: headers.cookie }).cookie;
    }

    const started = Date.now();
    const reqId = `${Date.now().toString(36)}-${Math.random().toString(36).slice(2, 8)}`;

    console.log(`\n>>> [${reqId}] ${clientReq.method} ${clientReq.url}`);
    console.log(`    → ${target.href}`);
    if (Object.keys(injected).length) console.log(`    injected:`, injected);

    const up = lib.request(
      {
        protocol: target.protocol,
        hostname: target.hostname,
        port: target.port || (isHttps ? 443 : 80),
        method: clientReq.method,
        path: target.pathname + target.search,
        headers,
        timeout: UPSTREAM_TIMEOUT_MS,
      },
      (upRes) => {
        const ctype = String(upRes.headers["content-type"] || "");
        const isStream = ctype.includes("text/event-stream");

        if (isStream) {
          const captured = [];
          let capturedBytes = 0;
          console.log(`<<< [${reqId}] ${upRes.statusCode} streaming (${ctype})`);
          clientRes.writeHead(upRes.statusCode, upRes.headers);
          clientRes.flushHeaders?.();
          upRes.on("data", (c) => {
            clientRes.write(c);
            if (capturedBytes < SSE_CAPTURE_CAP) {
              captured.push(c);
              capturedBytes += c.length;
            }
          });
          upRes.on("end", () => {
            clientRes.end();
            console.log(`<<< [${reqId}] stream end elapsed=${Date.now() - started}ms captured=${capturedBytes}B`);
            logEntry({
              id: reqId,
              ts: new Date().toISOString(),
              server: name,
              request: {
                method: clientReq.method,
                url: clientReq.url,
                upstream: target.href,
                headers: redactHeaders(clientReq.headers),
                injected,
                bodyBytes: bodyBuf?.length || 0,
                bodyText: bodyBuf?.toString("utf8") || "",
              },
              response: {
                status: upRes.statusCode,
                headers: redactHeaders(upRes.headers),
                bodyBytes: capturedBytes,
                streamed: true,
                bodyText: Buffer.concat(captured).toString("utf8").slice(0, SSE_CAPTURE_CAP),
              },
              elapsedMs: Date.now() - started,
            });
          });
          upRes.on("error", (err) => {
            console.error(`!!! [${reqId}] stream error:`, err.message);
            try {
              clientRes.destroy();
            } catch {}
          });
          return;
        }

        const chunks = [];
        upRes.on("data", (c) => chunks.push(c));
        upRes.on("end", () => {
          const buf = Buffer.concat(chunks);
          const elapsed = Date.now() - started;
          console.log(`<<< [${reqId}] ${upRes.statusCode} elapsed=${elapsed}ms bytes=${buf.length}`);
          const text = buf.toString("utf8");
          logEntry({
            id: reqId,
            ts: new Date().toISOString(),
            server: name,
            request: {
              method: clientReq.method,
              url: clientReq.url,
              upstream: target.href,
              headers: redactHeaders(clientReq.headers),
              injected,
              bodyBytes: bodyBuf?.length || 0,
              bodyText: bodyBuf?.toString("utf8") || "",
            },
            response: {
              status: upRes.statusCode,
              headers: redactHeaders(upRes.headers),
              bodyBytes: buf.length,
              bodyText: text.slice(0, 20000),
            },
            elapsedMs: elapsed,
          });
          clientRes.writeHead(upRes.statusCode, upRes.headers);
          clientRes.end(buf);
        });
      }
    );

    up.on("timeout", () => up.destroy(new Error("upstream timeout")));
    up.on("error", (err) => {
      console.error(`!!! [${reqId}] upstream error:`, err.message);
      logEntry({
        id: reqId,
        ts: new Date().toISOString(),
        server: name,
        request: { method: clientReq.method, url: clientReq.url, upstream: target.href },
        error: err.message,
        elapsedMs: Date.now() - started,
      });
      if (!clientRes.headersSent) clientRes.writeHead(502, { "content-type": "application/json" });
      clientRes.end(JSON.stringify({ error: "upstream_error", detail: err.message, target: target.href }));
    });

    if (bodyBuf?.length) up.write(bodyBuf);
    up.end();
  }

  const server = http.createServer((req, res) => {
    res.setHeader("Access-Control-Allow-Origin", "*");
    res.setHeader("Access-Control-Allow-Headers", "*");
    res.setHeader("Access-Control-Allow-Methods", "GET,POST,PUT,DELETE,OPTIONS,PATCH");
    if (req.method === "OPTIONS") {
      res.writeHead(204);
      res.end();
      return;
    }

    const pathname = req.url.split("?")[0];
    for (const l of local) {
      if (req.method === l.method && (typeof l.path === "string" ? l.path === pathname : l.path.test(pathname))) {
        l.handler(req, res, pathname);
        return;
      }
    }

    if (req.method === "GET" && (pathname === "/" || pathname === "/health" || pathname === "/__xm2api")) {
      sendJson(res, 200, typeof meta === "function" ? meta() : { ok: true, name, port });
      return;
    }

    const chunks = [];
    req.on("data", (c) => chunks.push(c));
    req.on("end", () => forward(req, res, Buffer.concat(chunks)));
  });

  return { server, forward, logEntry, port, host, name };
}

export function listen(routeServer, { onReady } = {}) {
  const { server, host, port } = routeServer;
  server.listen(port, host, () => {
    console.log(`${routeServer.name} listening on http://${host}:${port}`);
    if (onReady) onReady();
  });
  return server;
}
