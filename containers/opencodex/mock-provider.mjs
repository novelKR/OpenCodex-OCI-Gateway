#!/usr/bin/env bun

// Network-local, non-billing protocol fixture used only by image CI/canaries.
// It intentionally accepts no credentials and never connects outside its
// dedicated container network.

const encoder = new TextEncoder();
const MODEL = "runtime-contract-model";
const CANCELLATION_MARKER = "runtime cancellation contract";
const cancellation = { schema: 1, started: 0, cancelled: 0, completed: 0 };

function json(value, status = 200) {
  return Response.json(value, { status });
}

function chatCompletion(stream) {
  const id = "chatcmpl-runtime-contract";
  if (!stream) {
    return json({
      id,
      object: "chat.completion",
      created: 1,
      model: MODEL,
      choices: [{ index: 0, message: { role: "assistant", content: "runtime contract ok" }, finish_reason: "stop" }],
      usage: { prompt_tokens: 1, completion_tokens: 3, total_tokens: 4 },
    });
  }
  const frames = [
    { id, object: "chat.completion.chunk", created: 1, model: MODEL, choices: [{ index: 0, delta: { role: "assistant" }, finish_reason: null }] },
    { id, object: "chat.completion.chunk", created: 1, model: MODEL, choices: [{ index: 0, delta: { content: "runtime contract ok" }, finish_reason: null }] },
    { id, object: "chat.completion.chunk", created: 1, model: MODEL, choices: [{ index: 0, delta: {}, finish_reason: "stop" }], usage: { prompt_tokens: 1, completion_tokens: 3, total_tokens: 4 } },
  ];
  return new Response(new ReadableStream({
    start(controller) {
      for (const frame of frames) controller.enqueue(encoder.encode(`data: ${JSON.stringify(frame)}\n\n`));
      controller.enqueue(encoder.encode("data: [DONE]\n\n"));
      controller.close();
    },
  }), {
    headers: {
      "content-type": "text/event-stream; charset=utf-8",
      "cache-control": "no-cache",
    },
  });
}

function cancellationCompletion(request) {
  const id = "chatcmpl-runtime-cancellation";
  cancellation.started += 1;
  let timer;
  let settled = false;
  const cancelled = () => {
    if (settled) return;
    settled = true;
    clearInterval(timer);
    cancellation.cancelled += 1;
  };
  request.signal.addEventListener("abort", cancelled, { once: true });
  return new Response(new ReadableStream({
    start(controller) {
      controller.enqueue(encoder.encode(`data: ${JSON.stringify({
        id,
        object: "chat.completion.chunk",
        created: 1,
        model: MODEL,
        choices: [{ index: 0, delta: { role: "assistant" }, finish_reason: null }],
      })}\n\n`));
      let ticks = 0;
      timer = setInterval(() => {
        if (settled) return;
        ticks += 1;
        if (ticks < 240) {
          controller.enqueue(encoder.encode(`data: ${JSON.stringify({
            id,
            object: "chat.completion.chunk",
            created: 1,
            model: MODEL,
            choices: [{ index: 0, delta: { content: " " }, finish_reason: null }],
          })}\n\n`));
          return;
        }
        settled = true;
        clearInterval(timer);
        cancellation.completed += 1;
        controller.enqueue(encoder.encode(`data: ${JSON.stringify({
          id,
          object: "chat.completion.chunk",
          created: 1,
          model: MODEL,
          choices: [{ index: 0, delta: {}, finish_reason: "stop" }],
        })}\n\n`));
        controller.enqueue(encoder.encode("data: [DONE]\n\n"));
        controller.close();
      }, 250);
    },
    cancel() {
      cancelled();
    },
  }), {
    headers: {
      "content-type": "text/event-stream; charset=utf-8",
      "cache-control": "no-cache",
    },
  });
}

Bun.serve({
  hostname: "0.0.0.0",
  port: 18080,
  async fetch(request) {
    const url = new URL(request.url);
    if (request.method === "GET" && url.pathname === "/healthz") {
      return json({ status: "ok" });
    }
    if (request.method === "GET" && url.pathname === "/v1/models") {
      return json({ object: "list", data: [{ id: MODEL, object: "model", created: 1, owned_by: "fixture" }] });
    }
    if (request.method === "GET" && url.pathname === "/runtime-contract/cancellation") {
      return json(cancellation);
    }
    if (request.method === "POST" && url.pathname === "/v1/chat/completions") {
      let body;
      try {
        body = await request.json();
      } catch {
        return json({ error: { message: "invalid JSON" } }, 400);
      }
      if (body?.model !== MODEL || !Array.isArray(body?.messages)) {
        return json({ error: { message: "unexpected request" } }, 400);
      }
      if (body.stream === true && JSON.stringify(body.messages).includes(CANCELLATION_MARKER)) {
        return cancellationCompletion(request);
      }
      return chatCompletion(body.stream === true);
    }
    return json({ error: { message: "not found" } }, 404);
  },
});
