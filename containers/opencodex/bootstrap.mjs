#!/usr/bin/env bun

import net from "node:net";

export const BOOTSTRAP_SOCKET = "/run/opencodex/bootstrap.sock";
export const MAX_FRAME_BYTES = 4096;
const TOKEN_PATTERN = /^[A-Za-z0-9_-]{43}$/;
const EXPECTED_KEYS = ["admin_auth_token", "api_auth_token", "schema"];

function fail(message) {
  throw new Error(`bootstrap rejected: ${message}`);
}

function decodeBase64url(value) {
  if (!TOKEN_PATTERN.test(value)) fail("invalid token encoding");
  const decoded = Buffer.from(value, "base64url");
  const canonical = decoded.toString("base64url");
  const valid = decoded.length === 32 && canonical === value;
  decoded.fill(0);
  if (!valid) fail("invalid token encoding");
}

export function decodeEnvelope(payload) {
  if (!Buffer.isBuffer(payload) || payload.length === 0 || payload.length > MAX_FRAME_BYTES) {
    fail("invalid envelope size");
  }
  let value;
  const serialized = payload.toString("utf8");
  try {
    value = JSON.parse(serialized);
  } catch {
    fail("invalid JSON envelope");
  }
  if (value === null || Array.isArray(value) || typeof value !== "object") {
    fail("envelope must be an object");
  }
  const keys = Object.keys(value).sort();
  if (keys.length !== EXPECTED_KEYS.length || keys.some((key, index) => key !== EXPECTED_KEYS[index])) {
    fail("unsupported envelope field");
  }
  if (value.schema !== 1) fail("unsupported envelope schema");
  if (typeof value.api_auth_token !== "string" || typeof value.admin_auth_token !== "string") {
    fail("tokens must be strings");
  }
  decodeBase64url(value.api_auth_token);
  decodeBase64url(value.admin_auth_token);
  if (value.api_auth_token === value.admin_auth_token) fail("tokens must be distinct");
  const canonical = JSON.stringify({
    schema: 1,
    api_auth_token: value.api_auth_token,
    admin_auth_token: value.admin_auth_token,
  });
  if (serialized !== canonical) fail("envelope is not canonical");
  return {
    apiAuthToken: value.api_auth_token,
    adminAuthToken: value.admin_auth_token,
  };
}

function encodeFrame(value) {
  const payload = Buffer.from(JSON.stringify(value), "utf8");
  const header = Buffer.alloc(4);
  header.writeUInt32BE(payload.length, 0);
  return Buffer.concat([header, payload]);
}

export async function receiveEnvelope(socketPath = BOOTSTRAP_SOCKET) {
  return await new Promise((resolve, reject) => {
    const socket = net.createConnection({ path: socketPath });
    const chunks = [];
    let total = 0;
    let expected = null;
    let settled = false;
    const finish = (error, value) => {
      if (settled) return;
      settled = true;
      socket.destroy();
      for (const chunk of chunks) chunk.fill(0);
      if (error) reject(error);
      else resolve(value);
    };
    socket.setTimeout(10_000, () => finish(new Error("bootstrap socket timed out")));
    socket.on("error", () => finish(new Error("bootstrap socket unavailable")));
    socket.on("end", () => {
      if (!settled) finish(new Error("bootstrap socket closed before a complete frame"));
    });
    socket.on("data", (chunk) => {
      if (settled) return;
      chunks.push(chunk);
      total += chunk.length;
      if (total > MAX_FRAME_BYTES + 4) {
        finish(new Error("bootstrap frame exceeds limit"));
        return;
      }
      const joined = Buffer.concat(chunks, total);
      if (expected === null && joined.length >= 4) {
        expected = joined.readUInt32BE(0);
        if (expected === 0 || expected > MAX_FRAME_BYTES) {
          joined.fill(0);
          finish(new Error("bootstrap frame length is invalid"));
          return;
        }
      }
      if (expected === null || joined.length < expected + 4) {
        joined.fill(0);
        return;
      }
      if (joined.length !== expected + 4) {
        joined.fill(0);
        finish(new Error("bootstrap frame has trailing data"));
        return;
      }
      const payload = Buffer.from(joined.subarray(4));
      joined.fill(0);
      let envelope;
      try {
        envelope = decodeEnvelope(payload);
      } catch (error) {
        payload.fill(0);
        finish(error);
        return;
      }
      payload.fill(0);
      socket.write(encodeFrame({ schema: 1, accepted: true }), (error) => {
        if (error) finish(new Error("bootstrap acknowledgement failed"));
        else finish(null, envelope);
      });
    });
  });
}

async function main() {
  let envelope;
  try {
    envelope = await receiveEnvelope();
  } catch (error) {
    process.stderr.write(`${error instanceof Error ? error.message : "bootstrap failed"}\n`);
    return 78;
  }

  const executable = "/opt/opencodex/node_modules/.bin/ocx";
  const child = Bun.spawn([executable, "start", "--port", "10100"], {
    cwd: "/var/lib/opencodex",
    env: {
      ...process.env,
      OPENCODEX_API_AUTH_TOKEN: envelope.apiAuthToken,
      OPENCODEX_ADMIN_AUTH_TOKEN: envelope.adminAuthToken,
    },
    stdin: "inherit",
    stdout: "inherit",
    stderr: "inherit",
  });
  envelope.apiAuthToken = "";
  envelope.adminAuthToken = "";
  envelope = null;

  const forward = (signal) => {
    try {
      child.kill(signal);
    } catch {
      // The child may already have exited.
    }
  };
  process.on("SIGTERM", () => forward("SIGTERM"));
  process.on("SIGINT", () => forward("SIGINT"));
  return await child.exited;
}

if (import.meta.main) {
  process.exitCode = await main();
}
