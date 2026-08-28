import { createConnection } from "node:net";
import { getConfigDir, loadConfig } from "./package/src/config";
import { restoreNativeCodexAsync } from "./package/src/codex/inject";
import { stripGrokConfig } from "./package/src/grok/inject";
import {
  diagnoseService,
  prepareServiceInstall,
  proxyStillLiveAfterStop,
  uninstallServiceIfInstalled,
} from "./package/src/service";
import { revertSystemEnv, uninstallShellHook } from "./package/src/server/system-env";
import { INTEGRATION_CLIENT_IDS } from "./package/src/integrations/registry";
import { readIntegrationState } from "./package/src/integrations/state";
import { disableIntegrationCoordinated } from "./package/src/integrations/writer";
import {
  restoreShim as restoreShimTransaction,
  shimPreflight as shimTransactionPreflight,
} from "./relay_preserve_v1_shim";

const SAFE_TOKEN = /^[a-z0-9_-]{1,64}$/;
const SCHEMA_VERSION = 1;

function requestedAdapterID(argv: string[]): string | null {
  let selected: string | null = null;
  for (let index = 0; index < argv.length; index += 1) {
    if (argv[index] !== "--adapter-id") continue;
    if (selected !== null || index + 1 >= argv.length) return null;
    selected = argv[index + 1];
    index += 1;
  }
  return selected !== null && SAFE_TOKEN.test(selected) ? selected : null;
}

const REQUESTED_ADAPTER_ID = requestedAdapterID(process.argv);
const ADAPTER_ID = REQUESTED_ADAPTER_ID ?? "invalid_adapter";

type ComponentStatus = "completed" | "unchanged" | "refused" | "failed";
type Component = { component: string; status: ComponentStatus; code?: string };

function component(componentName: string, status: ComponentStatus, code?: string): Component {
  const safeComponent = SAFE_TOKEN.test(componentName) ? componentName : "adapter";
  const safeCode = code && SAFE_TOKEN.test(code) ? code : undefined;
  return { component: safeComponent, status, ...(safeCode ? { code: safeCode } : {}) };
}

function receipt(
  operation: "relay_preserving_teardown_preflight" | "relay_preserving_teardown",
  status: "ready" | "completed" | "partial" | "failed",
  components: Component[],
) {
  return {
    schema_version: SCHEMA_VERSION,
    operation,
    adapter_id: ADAPTER_ID,
    status,
    data_preserved: true,
    config_root_removed: false,
    components: components.slice(0, 32),
  } as const;
}

function emit(value: { status?: string }): never {
  process.stdout.write(`${JSON.stringify(value)}\n`);
  process.exit(value.status === "ready" || value.status === "completed" ? 0 : value.status === "partial" ? 2 : 1);
}

function loopbackHost(value: unknown): string | null {
  if (value === undefined || value === "127.0.0.1" || value === "localhost" || value === "0.0.0.0") return "127.0.0.1";
  if (value === "::1" || value === "::") return "::1";
  return null;
}

async function observePortAbsent(host: string, port: number): Promise<"absent" | "live" | "unknown"> {
  const observeOnce = () => new Promise<"absent" | "live" | "unknown">((resolve) => {
    let settled = false;
    const finish = (result: "absent" | "live" | "unknown") => {
      if (settled) return;
      settled = true;
      socket.destroy();
      resolve(result);
    };
    const socket = createConnection({ host, port });
    socket.once("connect", () => finish("live"));
    socket.once("error", (error: NodeJS.ErrnoException) => finish(error.code === "ECONNREFUSED" ? "absent" : "unknown"));
    socket.setTimeout(750, () => finish("unknown"));
  });
  for (let attempt = 0; attempt < 3; attempt += 1) {
    const result = await observeOnce();
    if (result !== "absent") return result;
    if (attempt < 2) await Bun.sleep(200);
  }
  return "absent";
}

async function preflight(): Promise<ReturnType<typeof receipt>> {
  const components: Component[] = [];
  if (process.platform !== "darwin") {
    return receipt("relay_preserving_teardown_preflight", "failed", [component("platform", "refused", "platform_unsupported")]);
  }
  try {
    const config = loadConfig();
    const port = config.port ?? 10100;
    if (!Number.isSafeInteger(port) || port <= 0 || port > 65535 || loopbackHost(config.hostname) === null) {
      return receipt("relay_preserving_teardown_preflight", "failed", [component("config", "refused", "config_unsupported")]);
    }
    components.push(component("config", "completed"));
  } catch {
    return receipt("relay_preserving_teardown_preflight", "failed", [component("config", "failed", "config_unavailable")]);
  }
  try {
    const service = diagnoseService();
    if (!service.supported || service.conflict) {
      return receipt("relay_preserving_teardown_preflight", "failed", [component("service", "refused", "service_unverified")]);
    }
    components.push(component("service", "completed"));
  } catch {
    return receipt("relay_preserving_teardown_preflight", "failed", [component("service", "failed", "service_unverified")]);
  }
  const shim = shimTransactionPreflight(getConfigDir());
  if (shim.status === "refused") {
    return receipt("relay_preserving_teardown_preflight", "failed", [component("codex_shim", "refused", "shim_ownership_refused")]);
  }
  components.push(component("codex_shim", shim.status === "ready" ? "completed" : "unchanged"));
  return receipt("relay_preserving_teardown_preflight", "ready", components);
}

async function execute(): Promise<ReturnType<typeof receipt>> {
  const checked = await preflight();
  if (checked.status !== "ready") {
    return receipt("relay_preserving_teardown", "failed", checked.components);
  }
  const config = loadConfig();
  const port = config.port ?? 10100;
  const host = loopbackHost(config.hostname)!;
  const shim = shimTransactionPreflight(getConfigDir());
  const components: Component[] = [];

  try {
    await prepareServiceInstall("scheduler");
    const live = await proxyStillLiveAfterStop({ canRespawn: true });
    if (live !== null) {
      components.push(component("quiescence", "refused", "proxy_still_live"));
      return receipt("relay_preserving_teardown", "failed", components);
    }
    const observed = await observePortAbsent(host, port);
    if (observed !== "absent") {
      components.push(component("quiescence", "refused", observed === "live" ? "proxy_still_live" : "proxy_observation_failed"));
      return receipt("relay_preserving_teardown", "failed", components);
    }
    components.push(component("quiescence", "completed"));
  } catch {
    components.push(component("quiescence", "failed", "quiescence_failed"));
    return receipt("relay_preserving_teardown", "failed", components);
  }

  try {
    const changed = uninstallServiceIfInstalled();
    if (diagnoseService().installed) {
      components.push(component("service", "refused", "service_removal_failed"));
      return receipt("relay_preserving_teardown", "partial", components);
    }
    components.push(component("service", changed ? "completed" : "unchanged"));
  } catch {
    components.push(component("service", "failed", "service_removal_failed"));
    return receipt("relay_preserving_teardown", "partial", components);
  }

  try {
    const restored = await restoreNativeCodexAsync({ revalidateDesiredState: false });
    components.push(component("native_codex", restored.success ? "completed" : "refused", restored.success ? undefined : "codex_restore_refused"));
  } catch {
    components.push(component("native_codex", "failed", "codex_restore_failed"));
  }

  try {
    const grok = stripGrokConfig();
    components.push(component("grok", !grok.ok ? "refused" : grok.changed ? "completed" : "unchanged", grok.ok ? undefined : "grok_restore_refused"));
  } catch {
    components.push(component("grok", "failed", "grok_restore_failed"));
  }

  for (const clientId of INTEGRATION_CLIENT_IDS) {
    const name = `client_${String(clientId).replaceAll("-", "_")}`;
    try {
      const input = { clientId, models: [], config, port };
      const state = readIntegrationState(input);
      if (state.state === "absent") {
        components.push(component(name, "unchanged"));
      } else if (state.state === "conflict" || state.state === "unsafe") {
        components.push(component(name, "refused", "integration_ownership_refused"));
      } else {
        const outcome = await disableIntegrationCoordinated(input);
        components.push(component(name, outcome.ok ? (outcome.changed ? "completed" : "unchanged") : "refused", outcome.ok ? undefined : "integration_disable_refused"));
      }
    } catch {
      components.push(component(name, "failed", "integration_error"));
    }
  }

  try {
    const environment = revertSystemEnv();
    const benign = environment.reason === "no tracking file" || environment.reason === "not macOS";
    components.push(component("system_environment", environment.reverted ? "completed" : benign ? "unchanged" : "refused", environment.reverted || benign ? undefined : "environment_ownership_refused"));
  } catch {
    components.push(component("system_environment", "failed", "environment_error"));
  }

  try {
    const hook = uninstallShellHook();
    const benign = hook.reason === "not installed" || hook.reason === "not macOS";
    components.push(component("shell_hook", hook.removed ? "completed" : benign ? "unchanged" : "refused", hook.removed || benign ? undefined : "shell_hook_refused"));
  } catch {
    components.push(component("shell_hook", "failed", "shell_hook_error"));
  }

  if (shim.status === "ready") {
    const restored = restoreShimTransaction(getConfigDir(), shim.proof);
    components.push(component("codex_shim", restored.ok ? (restored.changed ? "completed" : "unchanged") : "refused", restored.ok ? undefined : "shim_restore_refused"));
  } else {
    components.push(component("codex_shim", "unchanged"));
  }

  const hasProblem = components.some((item) => item.status === "refused" || item.status === "failed");
  return receipt("relay_preserving_teardown", hasProblem ? "partial" : "completed", components);
}

console.log = () => {};
console.info = () => {};
console.warn = () => {};
console.error = () => {};

const mode = process.argv.includes("--preflight") ? "preflight" : process.argv.includes("--execute") ? "execute" : "invalid";
if (REQUESTED_ADAPTER_ID === null) {
  emit(receipt("relay_preserving_teardown_preflight", "failed", [component("adapter", "refused", "invalid_request")]));
}
if (mode === "preflight") emit(await preflight());
if (mode === "execute") emit(await execute());
emit(receipt("relay_preserving_teardown_preflight", "failed", [component("adapter", "refused", "invalid_request")]));
