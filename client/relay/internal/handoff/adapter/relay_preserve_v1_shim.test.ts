import { afterEach, describe, expect, test } from "bun:test";
import {
  chmodSync,
  existsSync,
  lstatSync,
  mkdirSync,
  mkdtempSync,
  readdirSync,
  readFileSync,
  readlinkSync,
  realpathSync,
  rmSync,
  symlinkSync,
  unlinkSync,
  writeFileSync,
} from "node:fs";
import { join } from "node:path";
import { tmpdir } from "node:os";
import { restoreShim, shimPreflight } from "./relay_preserve_v1_shim";

const roots: string[] = [];

afterEach(() => {
  for (const root of roots.splice(0)) rmSync(root, { recursive: true, force: true });
});

type FixtureFile = { wrapperPath: string; backupPath: string; original: string; wrapper: string };

function fixture(count = 1, symlinkBackup = false): { root: string; configDir: string; files: FixtureFile[] } {
  const base = realpathSync(tmpdir());
  const root = mkdtempSync(join(base, "relay-shim-test-"));
  roots.push(root);
  const configDir = join(root, "config");
  const binDir = join(root, "bin");
  mkdirSync(configDir, { mode: 0o700 });
  mkdirSync(binDir, { mode: 0o700 });
  const files: FixtureFile[] = [];
  const stateFiles: Array<Record<string, string>> = [];
  for (let index = 0; index < count; index += 1) {
    const wrapperPath = join(binDir, `codex-${index}`);
    const backupPath = `${wrapperPath}.opencodex-real`;
    const wrapper = `#!/bin/sh\n# opencodex codex autostart shim\nensure ${index}\n`;
    const original = `#!/bin/sh\necho native-${index}\n`;
    writeFileSync(wrapperPath, wrapper, { mode: 0o700 });
    if (symlinkBackup) {
      const target = join(binDir, `native-${index}`);
      writeFileSync(target, original, { mode: 0o700 });
      symlinkSync(`native-${index}`, backupPath);
    } else {
      writeFileSync(backupPath, original, { mode: 0o700 });
    }
    files.push({ wrapperPath, backupPath, original, wrapper });
    stateFiles.push({ wrapperPath, originalPath: wrapperPath, backupPath });
  }
  writeFileSync(join(configDir, "codex-shim.json"), JSON.stringify({ platform: "darwin", wrappers: stateFiles }), { mode: 0o600 });
  return { root, configDir, files };
}

function proof(configDir: string) {
  const checked = shimPreflight(configDir);
  expect(checked.status).toBe("ready");
  if (checked.status !== "ready") throw new Error("fixture preflight failed");
  return checked.proof;
}

function lockOwnerRecord(configDir: string): Record<string, unknown> {
  const lock = join(configDir, "codex-shim.autorestore.lock");
  const owners = readdirSync(lock).filter((entry) => entry.endsWith(".json"));
  expect(owners.length).toBe(1);
  return JSON.parse(readFileSync(join(lock, owners[0]), "utf8")) as Record<string, unknown>;
}

describe("Relay-owned atomic shim restoration", () => {
  test("restores regular backups as one verified batch", () => {
    const value = fixture(2);
    const result = restoreShim(value.configDir, proof(value.configDir));
    expect(result).toEqual({ ok: true, changed: true });
    for (const file of value.files) {
      expect(readFileSync(file.wrapperPath, "utf8")).toBe(file.original);
      expect(existsSync(file.backupPath)).toBe(false);
    }
    expect(existsSync(join(value.configDir, "codex-shim.json"))).toBe(false);
    expect(existsSync(join(value.configDir, "codex-shim.autorestore.lock"))).toBe(false);
  });

  test("restores a symlink with its raw and followed target evidence", () => {
    const value = fixture(1, true);
    const expectedTarget = readlinkSync(value.files[0].backupPath);
    const result = restoreShim(value.configDir, proof(value.configDir));
    expect(result).toEqual({ ok: true, changed: true });
    expect(lstatSync(value.files[0].wrapperPath).isSymbolicLink()).toBe(true);
    expect(readlinkSync(value.files[0].wrapperPath)).toBe(expectedTarget);
    expect(readFileSync(value.files[0].wrapperPath, "utf8")).toBe(value.files[0].original);
  });

  test("writes an OpenCodex-compatible canonical lock owner record", () => {
    const value = fixture(1);
    let owner: Record<string, unknown> | undefined;
    const result = restoreShim(value.configDir, proof(value.configDir), {
      afterLockAcquired: () => { owner = lockOwnerRecord(value.configDir); },
    });
    expect(result).toEqual({ ok: true, changed: true });
    expect(owner?.version).toBe(1);
    expect(typeof owner?.token).toBe("string");
    expect(typeof owner?.pid === "number" && Number.isSafeInteger(owner.pid)).toBe(true);
    expect(typeof owner?.createdAt).toBe("number");
    expect(typeof owner?.createdAt === "number" && Number.isFinite(owner.createdAt)).toBe(true);
  });

  test("refuses a missing backup before changing wrappers", () => {
    const value = fixture(1);
    const initial = proof(value.configDir);
    const result = restoreShim(value.configDir, initial, {
      afterLockAcquired: () => unlinkSync(value.files[0].backupPath),
    });
    expect(result.ok).toBe(false);
    expect(readFileSync(value.files[0].wrapperPath, "utf8")).toBe(value.files[0].wrapper);
    expect(existsSync(join(value.configDir, "codex-shim.autorestore.lock"))).toBe(false);
  });

  test("rolls back all wrappers when the second publication fails", () => {
    const value = fixture(2);
    const result = restoreShim(value.configDir, proof(value.configDir), {
      beforePublish: (index) => { if (index === 1) throw new Error("injected publication failure"); },
    });
    expect(result).toEqual({ ok: false });
    for (const file of value.files) {
      expect(readFileSync(file.wrapperPath, "utf8")).toBe(file.wrapper);
      expect(existsSync(file.backupPath)).toBe(true);
    }
    expect(existsSync(join(value.configDir, "codex-shim.json"))).toBe(true);
    expect(existsSync(join(value.configDir, "codex-shim.autorestore.lock"))).toBe(false);
  });

  test("uses no-replace publication and retains recovery evidence on a destination race", () => {
    const value = fixture(1);
    const result = restoreShim(value.configDir, proof(value.configDir), {
      beforePublish: () => writeFileSync(value.files[0].wrapperPath, "racer\n", { mode: 0o600 }),
    });
    expect(result).toEqual({ ok: false, recoveryRequired: true });
    expect(readFileSync(value.files[0].wrapperPath, "utf8")).toBe("racer\n");
    expect(existsSync(value.files[0].backupPath)).toBe(true);
    expect(existsSync(join(value.configDir, "codex-shim.json"))).toBe(true);
    const lock = join(value.configDir, "codex-shim.autorestore.lock");
    expect(existsSync(lock)).toBe(true);
    expect(readdirSync(lock)).toContain("relay-recovery-required");
    expect(typeof lockOwnerRecord(value.configDir).createdAt).toBe("number");
  });

  test("retains staging and lock when rollback cannot be proven", () => {
    const value = fixture(1);
    const result = restoreShim(value.configDir, proof(value.configDir), {
      afterPublish: () => { throw new Error("force rollback"); },
      beforeRollback: () => {
        const entries = Array.from(new Bun.Glob(".codex-shim.relay-rollback-*").scanSync({ cwd: join(value.root, "bin") }));
        expect(entries.length).toBe(1);
        unlinkSync(join(value.root, "bin", entries[0]));
      },
    });
    expect(result).toEqual({ ok: false, recoveryRequired: true });
    expect(existsSync(join(value.configDir, "codex-shim.autorestore.lock"))).toBe(true);
    expect(existsSync(join(value.configDir, "codex-shim.json"))).toBe(true);
  });

  test("honors the canonical OpenCodex lock boundary", () => {
    const value = fixture(1);
    const lock = join(value.configDir, "codex-shim.autorestore.lock");
    mkdirSync(lock, { mode: 0o700 });
    chmodSync(lock, 0o700);
    const token = "upstream-owner";
    writeFileSync(join(lock, `${token}.json`), `${JSON.stringify({
      version: 1,
      token,
      pid: process.pid,
      createdAt: Date.now(),
    })}\n`, { mode: 0o600 });
    const result = restoreShim(value.configDir, proof(value.configDir));
    expect(result).toEqual({ ok: false });
    expect(readFileSync(value.files[0].wrapperPath, "utf8")).toBe(value.files[0].wrapper);
  });
});
