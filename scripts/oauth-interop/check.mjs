// OAuth interop self-check for `mcplex serve --oauth`, driven by the official MCP TypeScript SDK client.
// It performs what an OAuth-only MCP client such as Gemini does (discovery, dynamic client registration, PKCE S256,
// consent, code exchange, MCP calls, refresh) against your PUBLIC URL, then revokes the test grant.
//
//   cd scripts/oauth-interop && npm install
//   MCPLEX_TOKEN=<owner secret> node check.mjs https://mcp.example.com
import { Client } from "@modelcontextprotocol/sdk/client/index.js";
import { StreamableHTTPClientTransport } from "@modelcontextprotocol/sdk/client/streamableHttp.js";
import {
  UnauthorizedError,
  discoverAuthorizationServerMetadata,
  refreshAuthorization,
} from "@modelcontextprotocol/sdk/client/auth.js";

const base = (process.argv[2] || "").replace(/\/+$/, "");
const mcpPath = process.argv[3] || "/mcp";
const owner = process.env.MCPLEX_TOKEN || "";
if (!base || !owner) {
  console.error("usage: MCPLEX_TOKEN=<owner secret> node check.mjs <public-url> [mcp-path]");
  process.exit(2);
}

// Same redirect URI family Gemini uses; it is never actually visited (the script reads the Location header).
const REDIRECT = "https://oauth-redirect.googleusercontent.com/r/mcplex-interop-check";

class Provider {
  get redirectUrl() { return REDIRECT; }
  get clientMetadata() {
    return {
      client_name: "MCPlex interop check",
      redirect_uris: [REDIRECT],
      grant_types: ["authorization_code", "refresh_token"],
      response_types: ["code"],
      token_endpoint_auth_method: "client_secret_basic",
    };
  }
  clientInformation() { return this.client; }
  saveClientInformation(c) { this.client = c; }
  tokens() { return this.tok; }
  saveTokens(t) { this.tok = t; }
  redirectToAuthorization(u) { this.authUrl = u; }
  saveCodeVerifier(v) { this.verifier = v; }
  codeVerifier() { return this.verifier; }
}

const step = (name, ok, detail = "") => {
  console.log(`${ok ? "PASS" : "FAIL"}  ${name}${detail ? "  — " + detail : ""}`);
  if (!ok) process.exitCode = 1;
  return ok;
};
const decode = (s) => s.replace(/&#(\d+);/g, (_, n) => String.fromCharCode(+n))
  .replace(/&quot;/g, '"').replace(/&lt;/g, "<").replace(/&gt;/g, ">").replace(/&amp;/g, "&");

const provider = new Provider();
const mcpUrl = new URL(base + mcpPath);

try {
  await new Client({ name: "mcplex-interop", version: "1.0.0" })
    .connect(new StreamableHTTPClientTransport(mcpUrl, { authProvider: provider }));
  step("unauthenticated connect is rejected", false, "server accepted a request without a token");
  process.exit(1);
} catch (e) {
  if (!(e instanceof UnauthorizedError)) {
    step("discovery + dynamic client registration", false, e.message);
    process.exit(1);
  }
}
step("discovery + dynamic client registration", !!provider.client?.client_id, provider.client?.client_id);
const au = new URL(provider.authUrl);
step("authorize URL uses PKCE S256 + resource", au.searchParams.get("code_challenge_method") === "S256" && !!au.searchParams.get("resource"), au.origin + au.pathname);

const page = await (await fetch(provider.authUrl)).text();
const form = new URLSearchParams();
for (const m of page.matchAll(/<input type="hidden" name="([^"]+)" value="([^"]*)">/g)) form.set(m[1], decode(m[2]));
form.set("owner_secret", owner);
form.set("action", "approve");
const res = await fetch(base + "/oauth/authorize", { method: "POST", body: form, redirect: "manual", headers: { Origin: base } });
const loc = res.headers.get("location") ? new URL(res.headers.get("location")) : null;
if (!step("owner approval on the consent page", res.status === 302 && !!loc?.searchParams.get("code"), `HTTP ${res.status}`)) process.exit(1);

const transport = new StreamableHTTPClientTransport(mcpUrl, { authProvider: provider });
await transport.finishAuth(loc.searchParams.get("code"));
step("authorization code exchange", !!provider.tok?.access_token && !!provider.tok?.refresh_token);

const client = new Client({ name: "mcplex-interop", version: "1.0.0" });
await client.connect(transport);
const tools = await client.listTools();
const annotated = tools.tools.every((t) => t.annotations && typeof t.annotations.readOnlyHint === "boolean");
step("tools/list with annotations", annotated, tools.tools.map((t) => t.name).join(", "));
const r = await client.callTool({ name: "exec", arguments: { code: "return 6 * 7" } });
step("tools/call exec", /\b42\b/.test(r.content?.[0]?.text || ""));

const meta = await discoverAuthorizationServerMetadata(base);
const refreshed = await refreshAuthorization(base, {
  metadata: meta, clientInformation: provider.client, refreshToken: provider.tok.refresh_token, resource: mcpUrl,
});
step("refresh keeps a stable refresh token (persistent linking)", refreshed.refresh_token === provider.tok.refresh_token);
await client.close();

const basic = Buffer.from(`${encodeURIComponent(provider.client.client_id)}:${encodeURIComponent(provider.client.client_secret)}`).toString("base64");
const rv = await fetch(meta.revocation_endpoint, {
  method: "POST",
  headers: { Authorization: `Basic ${basic}`, "Content-Type": "application/x-www-form-urlencoded" },
  body: new URLSearchParams({ token: refreshed.refresh_token }),
});
const after = await fetch(mcpUrl, {
  method: "POST",
  headers: { Authorization: `Bearer ${refreshed.access_token}`, "Content-Type": "application/json", Accept: "application/json, text/event-stream" },
  body: JSON.stringify({ jsonrpc: "2.0", id: 1, method: "ping" }),
});
step("revocation invalidates the test grant", rv.status === 200 && after.status === 401, `revoke=${rv.status} mcp=${after.status}`);
console.log(process.exitCode ? "\nSome checks failed." : `\nAll checks passed. Add ${mcpUrl} as a custom MCP app in Gemini.`);
