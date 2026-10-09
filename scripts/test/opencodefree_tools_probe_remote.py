"""Ground-truth tool-calling probe against the OpenCodeFree gateway itself.

Runs ON the domestic test host. The router's key is read out of the app
container and handed to node on stdin, so it never appears in argv, in the host
process table or in the output; it is redacted from every emitted body.

This bypasses the router's capability gate on purpose: it answers "does the
upstream model emit tool_calls at all?", which is what decides whether a
501 model_capability_unsupported verdict upstream of the router is true or a
stale/false negative.

Placeholder: __MODELS__ (comma-separated upstream model ids).
"""

import json
import subprocess

GATEWAY = "opencode-free-proxy-opencode-free-proxy-1"
APP = "nvidia-router-app-1"
MODELS = "__MODELS__"

NODE_SCRIPT = r"""
const http = require('http');
const MODELS = process.argv[1].split(',').map((m) => m.trim()).filter(Boolean);
let key = '';
process.stdin.on('data', (c) => { key += c; });
process.stdin.on('end', async () => {
  key = key.trim();
  const call = (body, stream) => new Promise((resolve) => {
    const data = JSON.stringify(body);
    const started = Date.now();
    const req = http.request(
      { host: '127.0.0.1', port: 6020, path: '/v1/chat/completions', method: 'POST',
        headers: { Authorization: 'Bearer ' + key, 'Content-Type': 'application/json',
                   Accept: stream ? 'text/event-stream' : 'application/json' } },
      (res) => {
        let out = '';
        const ttft = [];
        res.on('data', (c) => { ttft.push(Date.now() - started); out += c; });
        res.on('end', () => resolve({
          status: res.statusCode, ttft_ms: ttft[0] === undefined ? null : ttft[0],
          ms: Date.now() - started, body: out.slice(0, 1800),
        }));
      });
    req.on('error', (e) => resolve({ error: String(e).slice(0, 300) }));
    req.write(data);
    req.end();
  });

  const weather = { type: 'function', function: {
    name: 'weather', description: 'Return the weather for a city.',
    parameters: { type: 'object', properties: { city: { type: 'string' } }, required: ['city'] } } };
  const readFile = { type: 'function', function: {
    name: 'read_file', description: 'Read a file from the project.',
    parameters: { type: 'object', properties: { path: { type: 'string' } }, required: ['path'] } } };

  const summarise = (result) => {
    if (result.error) return { error: result.error };
    const entry = { status: result.status, ms: result.ms, ttft_ms: result.ttft_ms };
    let parsed = null;
    try { parsed = JSON.parse(result.body); } catch (e) { entry.unparsed_bytes = result.body.length; }
    if (parsed && parsed.error) {
      const message = String((parsed.error && parsed.error.message) || '').slice(0, 200);
      entry.error_message = message.replace(/\s+/g, ' ');
    }
    if (parsed && Array.isArray(parsed.choices)) {
      const choice = parsed.choices[0] || {};
      const message = choice.message || {};
      entry.finish_reason = choice.finish_reason || null;
      entry.content_chars = String(message.content || '').length;
      entry.reasoning_chars = String(message.reasoning_content || '').length;
      const calls = message.tool_calls || [];
      entry.tool_calls = calls.length;
      entry.tool_names = calls.map((c) => (c.function || {}).name).slice(0, 4);
      entry.tool_args_valid = calls.every((c) => {
        try { return typeof JSON.parse((c.function || {}).arguments) === 'object'; }
        catch (e) { return false; }
      });
      entry.tool_args_preview = calls.length ? String(calls[0].function.arguments || '').slice(0, 160) : null;
    }
    if (!entry.tool_calls && !entry.error_message && entry.unparsed_bytes === undefined) {
      entry.raw_head = String(result.body || '').slice(0, 200).replace(/\s+/g, ' ');
    }
    return entry;
  };

  for (const model of MODELS) {
    // probe_form_* mirror the router's own tools probe bodies
    // (internal/modelcatalog/service_probe.go marshalProbeToolsBody) verbatim,
    // so a verdict written by the router can be compared against the upstream.
    console.log('RESULT ' + JSON.stringify({ model, case: 'probe_form_required',
      ...summarise(await call({ model, messages: [{ role: 'user', content: 'Reply with exactly OK.' }],
        max_tokens: 256, tools: [weather], tool_choice: 'required' }, false)) }));
    console.log('RESULT ' + JSON.stringify({ model, case: 'probe_form_auto',
      ...summarise(await call({ model, messages: [{ role: 'user', content: 'You must call the weather tool.' }],
        max_tokens: 256, tools: [weather], tool_choice: 'auto' }, false)) }));
    console.log('RESULT ' + JSON.stringify({ model, case: 'agentic_tools',
      ...summarise(await call({ model, messages: [{ role: 'user', content: 'Read the file README.md using the tool, then report its first line.' }],
        max_tokens: 1024, tools: [readFile], tool_choice: 'auto' }, false)) }));
  }
});
"""


def emit(kind, **fields):
    fields["kind"] = kind
    print("R|" + json.dumps(fields, sort_keys=True, separators=(",", ":")), flush=True)


def main():
    key = ""
    try:
        with open("/opt/nvidia-router/.env", encoding="utf-8") as handle:
            for line in handle:
                if line.startswith("NVIDIA_ROUTER_OPENCODEFREE_AUTH_KEY="):
                    key = line.split("=", 1)[1].strip()
                    break
    except Exception:
        pass
    if not key:
        key = subprocess.run(
            ["docker", "exec", APP, "printenv", "NVIDIA_ROUTER_OPENCODEFREE_AUTH_KEY"],
            capture_output=True, text=True, timeout=60,
        ).stdout.strip()
    if not key:
        emit("meta", step="key", present=False)
        return 1
    result = subprocess.run(
        ["docker", "exec", "-i", GATEWAY, "node", "-e", NODE_SCRIPT, "--", MODELS],
        input=key + "\n", capture_output=True, text=True, timeout=900,
    )
    if result.returncode != 0:
        emit("meta", step="node", exit=result.returncode,
             stderr=result.stderr[:500].replace(key, "[redacted]"))
        return 1
    for line in result.stdout.splitlines():
        if line.startswith("RESULT "):
            emit("gateway", **json.loads(line[len("RESULT "):].replace(key, "[redacted]")))
    return 0


raise SystemExit(main())
