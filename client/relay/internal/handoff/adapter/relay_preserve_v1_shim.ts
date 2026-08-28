import { randomUUID } from "node:crypto";
import { dirname, extname, isAbsolute, join, normalize } from "node:path";
import {
  chmodSync,
  closeSync,
  constants,
  existsSync,
  fstatSync,
  linkSync,
  lstatSync,
  mkdirSync,
  openSync,
  readFileSync,
  readlinkSync,
  readSync,
  rmdirSync,
  statSync,
  symlinkSync,
  unlinkSync,
  writeSync,
} from "node:fs";

const STATE_LIMIT = 1024 * 1024;
const SHIM_PROBE_LIMIT = 16 * 1024;
const SHIM_MARKER = "opencodex codex autostart shim";
const LOCK_DIRECTORY = "codex-shim.autorestore.lock";
const LOCK_OWNER_SUFFIX = ".json";
const LOCK_RECOVERY_MARKER = "relay-recovery-required";

export type Fingerprint = {
  dev: number;
  ino: number;
  mode: number;
  size: number;
  mtimeMs: number;
  ctimeMs: number;
  kind: "file" | "symlink";
  target?: Omit<Fingerprint, "target">;
};

export type ShimFile = {
  wrapperPath: string;
  originalPath: string;
  backupPath: string;
  realPath?: string;
  preserveOnly?: boolean;
};

export type ShimState = ShimFile & { platform: string; wrappers?: ShimFile[] };
export type ShimFileProof = { file: ShimFile; wrapper: Fingerprint; backup: Fingerprint; backupLink?: string };
export type ShimProof = { statePath: string; state: ShimState; stateFingerprint: Fingerprint; files: ShimFileProof[] };
export type ShimPreflight = { status: "absent" } | { status: "ready"; proof: ShimProof } | { status: "refused" };

export type ShimRestoreHooks = {
  afterLockAcquired?: () => void;
  beforeStage?: (index: number) => void;
  beforeUnlink?: (index: number) => void;
  beforePublish?: (index: number) => void;
  afterPublish?: (index: number) => void;
  beforeCommit?: () => void;
  beforeRollback?: (index: number) => void;
};

type LockProof = {
  path: string;
  ownerPath: string;
  token: string;
  directory: Fingerprint;
  owner: Fingerprint;
  mutationMarker?: Fingerprint;
};

type StagedFile = ShimFileProof & { rollbackPath: string; unlinked: boolean; published: boolean };

export type ShimRestoreResult =
  | { ok: true; changed: boolean }
  | { ok: false; recoveryRequired?: boolean };

export function sameFingerprint(left: Fingerprint, right: Fingerprint): boolean {
  return left.dev === right.dev
    && left.ino === right.ino
    && left.mode === right.mode
    && left.size === right.size
    && left.mtimeMs === right.mtimeMs
    && left.ctimeMs === right.ctimeMs
    && left.kind === right.kind
    && (left.target === undefined && right.target === undefined
      || left.target !== undefined && right.target !== undefined
        && sameFingerprint(left.target as Fingerprint, right.target as Fingerprint));
}

function sameObject(left: Fingerprint, right: Fingerprint): boolean {
  return left.dev === right.dev && left.ino === right.ino && left.kind === right.kind;
}

function statFingerprint(path: string, follow: boolean): Omit<Fingerprint, "target"> | null {
  try {
    const stat = follow ? statSync(path) : lstatSync(path);
    if (follow ? !stat.isFile() : (!stat.isFile() && !stat.isSymbolicLink() && !stat.isDirectory())) return null;
    if (typeof process.geteuid === "function" && stat.uid !== process.geteuid()) return null;
    return {
      dev: stat.dev,
      ino: stat.ino,
      mode: stat.mode,
      size: stat.size,
      mtimeMs: stat.mtimeMs,
      ctimeMs: stat.ctimeMs,
      kind: stat.isSymbolicLink() ? "symlink" : "file",
    };
  } catch {
    return null;
  }
}

export function stableFingerprint(path: string): Fingerprint | null {
  const before = statFingerprint(path, false);
  if (!before || (before.kind !== "file" && before.kind !== "symlink")) return null;
  const targetBefore = before.kind === "symlink" ? statFingerprint(path, true) : undefined;
  if (before.kind === "symlink" && (!targetBefore || targetBefore.kind !== "file")) return null;
  const targetAfter = before.kind === "symlink" ? statFingerprint(path, true) : undefined;
  const after = statFingerprint(path, false);
  if (!after || !sameFingerprint(before as Fingerprint, after as Fingerprint)) return null;
  if (targetBefore && (!targetAfter || !sameFingerprint(targetBefore as Fingerprint, targetAfter as Fingerprint))) return null;
  return { ...before, ...(targetBefore ? { target: targetBefore } : {}) } as Fingerprint;
}

function stableDirectoryFingerprint(path: string): Fingerprint | null {
  try {
    const before = lstatSync(path);
    if (!before.isDirectory() || before.isSymbolicLink()) return null;
    if (typeof process.geteuid === "function" && before.uid !== process.geteuid()) return null;
    const after = lstatSync(path);
    const left: Fingerprint = { dev: before.dev, ino: before.ino, mode: before.mode, size: before.size, mtimeMs: before.mtimeMs, ctimeMs: before.ctimeMs, kind: "file" };
    const right: Fingerprint = { dev: after.dev, ino: after.ino, mode: after.mode, size: after.size, mtimeMs: after.mtimeMs, ctimeMs: after.ctimeMs, kind: "file" };
    return sameFingerprint(left, right) ? left : null;
  } catch {
    return null;
  }
}

function readStableRegular(path: string, maximum: number): { text: string; fingerprint: Fingerprint } | null {
  const noFollow = typeof constants.O_NOFOLLOW === "number" ? constants.O_NOFOLLOW : 0;
  let descriptor: number;
  try {
    descriptor = openSync(path, constants.O_RDONLY | noFollow);
  } catch {
    return null;
  }
  try {
    const before = fstatSync(descriptor);
    if (!before.isFile() || before.size < 0 || before.size > maximum) return null;
    if (typeof process.geteuid === "function" && before.uid !== process.geteuid()) return null;
    if ((before.mode & 0o022) !== 0) return null;
    const payload = Buffer.alloc(before.size);
    let offset = 0;
    while (offset < payload.length) {
      const count = readSync(descriptor, payload, offset, payload.length - offset, offset);
      if (count <= 0) return null;
      offset += count;
    }
    const extra = Buffer.alloc(1);
    if (readSync(descriptor, extra, 0, 1, offset) !== 0) return null;
    const after = fstatSync(descriptor);
    const fingerprint: Fingerprint = {
      dev: before.dev, ino: before.ino, mode: before.mode, size: before.size,
      mtimeMs: before.mtimeMs, ctimeMs: before.ctimeMs, kind: "file",
    };
    const afterFingerprint: Fingerprint = {
      dev: after.dev, ino: after.ino, mode: after.mode, size: after.size,
      mtimeMs: after.mtimeMs, ctimeMs: after.ctimeMs, kind: "file",
    };
    return sameFingerprint(fingerprint, afterFingerprint)
      ? { text: payload.toString("utf8"), fingerprint }
      : null;
  } finally {
    closeSync(descriptor);
  }
}

function canonicalAbsolute(path: string): boolean {
  return path.length > 1 && path.length <= 4096 && isAbsolute(path) && normalize(path) === path && !path.includes("\0");
}

function backupPathFor(path: string): string {
  const extension = extname(path);
  return extension ? `${path.slice(0, -extension.length)}.opencodex-real${extension}` : `${path}.opencodex-real`;
}

function filesOf(state: ShimState): ShimFile[] {
  return state.wrappers?.length ? state.wrappers : [{
    wrapperPath: state.wrapperPath,
    originalPath: state.originalPath,
    backupPath: state.backupPath,
    ...(state.realPath ? { realPath: state.realPath } : {}),
    ...(state.preserveOnly !== undefined ? { preserveOnly: state.preserveOnly } : {}),
  }];
}

function validShimFile(value: unknown): value is ShimFile {
  if (!value || typeof value !== "object") return false;
  const file = value as Record<string, unknown>;
  return typeof file.wrapperPath === "string"
    && typeof file.originalPath === "string"
    && typeof file.backupPath === "string"
    && (file.realPath === undefined || typeof file.realPath === "string")
    && (file.preserveOnly === undefined || typeof file.preserveOnly === "boolean");
}

export function shimPreflight(configDir: string): ShimPreflight {
  const statePath = join(configDir, "codex-shim.json");
  if (!existsSync(statePath)) return { status: "absent" };
  const read = readStableRegular(statePath, STATE_LIMIT);
  if (!read) return { status: "refused" };
  let state: ShimState;
  try {
    const parsed = JSON.parse(read.text) as unknown;
    if (!parsed || typeof parsed !== "object") return { status: "refused" };
    state = parsed as ShimState;
  } catch {
    return { status: "refused" };
  }
  if (state.platform !== "darwin") return { status: "refused" };
  if (state.wrappers !== undefined) {
    if (!Array.isArray(state.wrappers) || state.wrappers.length === 0 || state.wrappers.length > 8 ||
        !state.wrappers.every(validShimFile)) return { status: "refused" };
  } else if (!validShimFile(state)) {
    return { status: "refused" };
  }

  const seen = new Set<string>();
  const proofs: ShimFileProof[] = [];
  for (const file of filesOf(state)) {
    if (file.preserveOnly === true || !canonicalAbsolute(file.wrapperPath) || !canonicalAbsolute(file.originalPath)
      || !canonicalAbsolute(file.backupPath) || file.originalPath !== file.wrapperPath
      || file.backupPath !== backupPathFor(file.originalPath) || dirname(file.backupPath) !== dirname(file.originalPath)
      || seen.has(file.wrapperPath) || seen.has(file.backupPath)) return { status: "refused" };
    seen.add(file.wrapperPath);
    seen.add(file.backupPath);
    const wrapper = stableFingerprint(file.wrapperPath);
    const backup = stableFingerprint(file.backupPath);
    if (!wrapper || wrapper.kind !== "file" || !backup) return { status: "refused" };
    if ((wrapper.mode & 0o111) === 0 || (wrapper.mode & 0o022) !== 0 || (backup.mode & 0o022) !== 0) {
      return { status: "refused" };
    }
    const wrapperRead = readStableRegular(file.wrapperPath, SHIM_PROBE_LIMIT);
    if (!wrapperRead || !sameFingerprint(wrapper, wrapperRead.fingerprint)
      || !wrapperRead.text.includes(SHIM_MARKER) || !wrapperRead.text.includes("ensure")) {
      return { status: "refused" };
    }
    const backupLink = backup.kind === "symlink" ? readlinkSync(file.backupPath) : undefined;
    proofs.push({ file, wrapper, backup, ...(backupLink !== undefined ? { backupLink } : {}) });
  }
  return { status: "ready", proof: { statePath, state, stateFingerprint: read.fingerprint, files: proofs } };
}

function acquireLock(configDir: string): LockProof | null {
  const lockPath = join(configDir, LOCK_DIRECTORY);
  const token = randomUUID().replaceAll("-", "");
  const ownerPath = join(lockPath, `${token}${LOCK_OWNER_SUFFIX}`);
  try {
    mkdirSync(lockPath, { mode: 0o700 });
    chmodSync(lockPath, 0o700);
    const directory = stableDirectoryFingerprint(lockPath);
    if (!directory || (directory.mode & 0o777) !== 0o700) throw new Error("unsafe lock");
    const descriptor = openSync(ownerPath, constants.O_CREAT | constants.O_EXCL | constants.O_WRONLY, 0o600);
    try {
      const payload = Buffer.from(`${JSON.stringify({ version: 1, token, pid: process.pid, createdAt: Date.now() })}\n`);
      if (writeSync(descriptor, payload, 0, payload.length, 0) !== payload.length) throw new Error("short owner record");
    } finally {
      closeSync(descriptor);
    }
    const owner = stableFingerprint(ownerPath);
    if (!owner || owner.kind !== "file" || (owner.mode & 0o777) !== 0o600) throw new Error("unsafe owner record");
    return { path: lockPath, ownerPath, token, directory, owner };
  } catch {
    return null;
  }
}

function pinLockForMutation(lock: LockProof): boolean {
  const markerPath = join(lock.path, LOCK_RECOVERY_MARKER);
  let descriptor: number | null = null;
  try {
    const directory = stableDirectoryFingerprint(lock.path);
    const owner = stableFingerprint(lock.ownerPath);
    if (!directory || !owner || !sameObject(directory, lock.directory) || !sameFingerprint(owner, lock.owner)) {
      return false;
    }
    descriptor = openSync(markerPath, constants.O_CREAT | constants.O_EXCL | constants.O_WRONLY, 0o600);
    const payload = Buffer.from(`${JSON.stringify({ version: 1, token: lock.token, state: "mutation_in_progress" })}\n`);
    if (writeSync(descriptor, payload, 0, payload.length, 0) !== payload.length) return false;
    closeSync(descriptor);
    descriptor = null;
    const marker = stableFingerprint(markerPath);
    if (!marker || marker.kind !== "file" || (marker.mode & 0o777) !== 0o600) return false;
    lock.mutationMarker = marker;
    return true;
  } catch {
    return false;
  } finally {
    if (descriptor !== null) {
      try { closeSync(descriptor); } catch { /* the retained marker pins uncertain recovery */ }
    }
  }
}

function clearMutationPin(lock: LockProof): boolean {
  if (!lock.mutationMarker) return true;
  const markerPath = join(lock.path, LOCK_RECOVERY_MARKER);
  try {
    const marker = stableFingerprint(markerPath);
    if (!marker || !sameFingerprint(marker, lock.mutationMarker)) return false;
    unlinkSync(markerPath);
    lock.mutationMarker = undefined;
    return true;
  } catch {
    return false;
  }
}

function releaseLock(lock: LockProof): boolean {
  try {
    const directory = stableDirectoryFingerprint(lock.path);
    const owner = stableFingerprint(lock.ownerPath);
    if (!directory || !owner || !sameObject(directory, lock.directory) || !sameFingerprint(owner, lock.owner)) return false;
    const parsed = JSON.parse(readFileSync(lock.ownerPath, "utf8")) as Record<string, unknown>;
    if (parsed.version !== 1 || parsed.token !== lock.token
      || typeof parsed.pid !== "number" || !Number.isSafeInteger(parsed.pid) || parsed.pid <= 0
      || typeof parsed.createdAt !== "number" || !Number.isFinite(parsed.createdAt)) return false;
    unlinkSync(lock.ownerPath);
    rmdirSync(lock.path);
    return true;
  } catch {
    return false;
  }
}

function sameProof(left: ShimProof, right: ShimProof): boolean {
  if (!sameFingerprint(left.stateFingerprint, right.stateFingerprint)
    || JSON.stringify(left.state) !== JSON.stringify(right.state)
    || left.files.length !== right.files.length) return false;
  return left.files.every((expected, index) => {
    const actual = right.files[index];
    return !!actual && actual.file.wrapperPath === expected.file.wrapperPath
      && actual.file.backupPath === expected.file.backupPath
      && sameFingerprint(expected.wrapper, actual.wrapper)
      && sameFingerprint(expected.backup, actual.backup)
      && expected.backupLink === actual.backupLink;
  });
}

function publicationMatches(stage: StagedFile): boolean {
  const published = stableFingerprint(stage.file.originalPath);
  if (!published) return false;
  if (stage.backup.kind === "file") {
    const backup = stableFingerprint(stage.file.backupPath);
    return !!backup && sameObject(published, backup) && published.mode === backup.mode
      && published.size === backup.size && published.mtimeMs === backup.mtimeMs;
  }
  if (published.kind !== "symlink" || stage.backupLink === undefined) return false;
  try {
    return readlinkSync(stage.file.originalPath) === stage.backupLink
      && !!published.target && !!stage.backup.target && sameFingerprint(published.target as Fingerprint, stage.backup.target as Fingerprint);
  } catch {
    return false;
  }
}

function rollback(staged: StagedFile[], hooks: ShimRestoreHooks): boolean {
  let complete = true;
  for (let index = staged.length - 1; index >= 0; index -= 1) {
    const stage = staged[index];
    try {
      hooks.beforeRollback?.(index);
      if (!stage.unlinked) {
        unlinkSync(stage.rollbackPath);
        continue;
      }
      if (stage.published) {
        if (!publicationMatches(stage)) throw new Error("publication changed");
        unlinkSync(stage.file.originalPath);
      } else if (existsSync(stage.file.originalPath)) {
        throw new Error("destination occupied");
      }
      const rollbackProof = stableFingerprint(stage.rollbackPath);
      if (!rollbackProof || rollbackProof.kind !== "file") throw new Error("rollback staging changed");
      linkSync(stage.rollbackPath, stage.file.wrapperPath);
      const restored = stableFingerprint(stage.file.wrapperPath);
      if (!restored || !sameObject(restored, rollbackProof)) throw new Error("wrapper rollback failed");
      unlinkSync(stage.rollbackPath);
    } catch {
      complete = false;
    }
  }
  return complete;
}

export function restoreShim(configDir: string, initial: ShimProof, hooks: ShimRestoreHooks = {}): ShimRestoreResult {
  const lock = acquireLock(configDir);
  if (!lock) return { ok: false };
  let mutated = false;
  const staged: StagedFile[] = [];
  try {
    hooks.afterLockAcquired?.();
    const current = shimPreflight(configDir);
    if (current.status !== "ready" || !sameProof(initial, current.proof)) throw new Error("shim proof changed");

    for (let index = 0; index < current.proof.files.length; index += 1) {
      hooks.beforeStage?.(index);
      const proof = current.proof.files[index];
      const rollbackPath = join(dirname(proof.file.wrapperPath), `.codex-shim.relay-rollback-${lock.token}-${index}`);
      linkSync(proof.file.wrapperPath, rollbackPath);
      const wrapper = stableFingerprint(proof.file.wrapperPath);
      const rollbackProof = stableFingerprint(rollbackPath);
      if (!wrapper || !rollbackProof || !sameObject(wrapper, rollbackProof)) throw new Error("rollback staging failed");
      staged.push({ ...proof, wrapper, rollbackPath, unlinked: false, published: false });
    }

    const state = stableFingerprint(initial.statePath);
    if (!state || !sameFingerprint(state, initial.stateFingerprint)) throw new Error("state changed");
    for (const stage of staged) {
      const wrapper = stableFingerprint(stage.file.wrapperPath);
      const backup = stableFingerprint(stage.file.backupPath);
      if (!wrapper || !backup || !sameFingerprint(wrapper, stage.wrapper) || !sameFingerprint(backup, stage.backup)) {
        throw new Error("shim inputs changed");
      }
    }

    if (!pinLockForMutation(lock)) throw new Error("could not pin shim mutation lock");

    for (let index = 0; index < staged.length; index += 1) {
      const stage = staged[index];
      hooks.beforeUnlink?.(index);
      const wrapper = stableFingerprint(stage.file.wrapperPath);
      const backup = stableFingerprint(stage.file.backupPath);
      if (!wrapper || !backup || !sameFingerprint(wrapper, stage.wrapper) || !sameFingerprint(backup, stage.backup)) {
        throw new Error("shim inputs changed");
      }
      unlinkSync(stage.file.wrapperPath);
      stage.unlinked = true;
      mutated = true;
      hooks.beforePublish?.(index);
      if (stage.backup.kind === "symlink") {
        if (stage.backupLink === undefined) throw new Error("missing symlink proof");
        symlinkSync(stage.backupLink, stage.file.originalPath);
      } else {
        linkSync(stage.file.backupPath, stage.file.originalPath);
      }
      stage.published = true;
      if (!publicationMatches(stage)) throw new Error("publication verification failed");
      hooks.afterPublish?.(index);
    }

    if (!staged.every(publicationMatches)) throw new Error("batch verification failed");
    const finalState = stableFingerprint(initial.statePath);
    if (!finalState || !sameFingerprint(finalState, initial.stateFingerprint)) throw new Error("state changed");
    hooks.beforeCommit?.();

    for (const stage of staged) unlinkSync(stage.file.backupPath);
    unlinkSync(initial.statePath);
    for (const stage of staged) unlinkSync(stage.rollbackPath);
    if (!clearMutationPin(lock) || !releaseLock(lock)) return { ok: false, recoveryRequired: true };
    return { ok: true, changed: true };
  } catch {
    if (mutated) {
      const complete = rollback(staged, hooks);
      const released = complete && clearMutationPin(lock) && releaseLock(lock);
      return { ok: false, ...(!complete || !released ? { recoveryRequired: true } : {}) };
    }
    let stagingClean = true;
    for (const stage of staged.reverse()) {
      try { unlinkSync(stage.rollbackPath); } catch { stagingClean = false; }
    }
    const released = stagingClean && clearMutationPin(lock) && releaseLock(lock);
    return { ok: false, ...(!stagingClean || !released ? { recoveryRequired: true } : {}) };
  }
}
