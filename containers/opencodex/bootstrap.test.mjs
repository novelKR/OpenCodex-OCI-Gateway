import assert from "node:assert/strict";
import { test } from "node:test";

import { decodeEnvelope, MAX_FRAME_BYTES } from "./bootstrap.mjs";

const API = Buffer.alloc(32, 0).toString("base64url");
const ADMIN = Buffer.alloc(32, 1).toString("base64url");

function envelope(overrides = {}) {
  return Buffer.from(JSON.stringify({
    schema: 1,
    api_auth_token: API,
    admin_auth_token: ADMIN,
    ...overrides,
  }));
}

test("accepts only distinct 32-byte base64url tokens", () => {
  assert.deepEqual(decodeEnvelope(envelope()), {
    apiAuthToken: API,
    adminAuthToken: ADMIN,
  });
  assert.throws(() => decodeEnvelope(envelope({ admin_auth_token: API })), /distinct/);
  assert.throws(() => decodeEnvelope(envelope({ api_auth_token: "short" })), /encoding/);
  const noncanonicalAlias = `${API.slice(0, -1)}B`;
  assert.deepEqual(Buffer.from(noncanonicalAlias, "base64url"), Buffer.from(API, "base64url"));
  assert.throws(
    () => decodeEnvelope(envelope({ api_auth_token: noncanonicalAlias })),
    /encoding/,
  );
});

test("rejects unknown fields and schema revisions", () => {
  assert.throws(() => decodeEnvelope(envelope({ extra: true })), /unsupported envelope field/);
  assert.throws(() => decodeEnvelope(envelope({ schema: 2 })), /unsupported envelope schema/);
  assert.throws(
    () => decodeEnvelope(Buffer.from(`{"schema":1,"schema":1,"api_auth_token":"${API}","admin_auth_token":"${ADMIN}"}`)),
    /canonical/,
  );
});

test("rejects malformed and oversized payloads without including token bytes", () => {
  const secret = "S".repeat(43);
  for (const payload of [Buffer.from("{"), Buffer.alloc(MAX_FRAME_BYTES + 1)]) {
    assert.throws(
      () => decodeEnvelope(payload),
      (error) => error instanceof Error && !error.message.includes(secret),
    );
  }
});
