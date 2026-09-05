export interface ClaudeTestUsage {
  input_tokens?: number;
  output_tokens?: number;
  cache_read_input_tokens?: number;
  cache_creation_input_tokens?: number;
  cache_creation?: {
    ephemeral_5m_input_tokens?: number;
    ephemeral_1h_input_tokens?: number;
  };
}

export interface ClaudeTestDiagnostics {
  http_status?: number;
  duration_ms?: number;
  headers_ms?: number;
  first_content_ms?: number;
  model: string;
  response_model?: string;
  fingerprint_mode?: string;
  request_id?: string;
  organization_id?: string;
  message_id?: string;
  stop_reason?: string;
  error_type?: string;
  usage?: ClaudeTestUsage;
  response_headers?: Array<{ name: string; value: string }>;
  response_body?: string;
  body_truncated?: boolean;
}

export interface ClaudeTestEvent {
  type: "test_start" | "content" | "diagnostics" | "test_complete" | "error";
  model?: string;
  text?: string;
  error?: string;
  success?: boolean;
  diagnostics?: ClaudeTestDiagnostics;
}

const eventTypes = new Set(["test_start", "content", "diagnostics", "test_complete", "error"]);

export function createClaudeTestEventParser(onEvent: (event: ClaudeTestEvent) => void) {
  let buffer = "";
  const processFrame = (frame: string) => {
    const data = frame.split(/\r?\n/)
      .filter((line) => line.startsWith("data:"))
      .map((line) => line.slice(5).replace(/^ /, ""))
      .join("\n");
    if (!data || data === "[DONE]") return;
    let event: ClaudeTestEvent;
    try {
      event = JSON.parse(data) as ClaudeTestEvent;
    } catch {
      return;
    }
    if (event && eventTypes.has(event.type)) onEvent(event);
  };
  return {
    feed(chunk: string) {
      buffer += chunk;
      let boundary: RegExpExecArray | null;
      while ((boundary = /\r?\n\r?\n/.exec(buffer)) !== null) {
        const frame = buffer.slice(0, boundary.index);
        buffer = buffer.slice(boundary.index + boundary[0].length);
        processFrame(frame);
      }
    },
    finish() {
      if (buffer.trim()) processFrame(buffer);
      buffer = "";
    },
  };
}

// The terminal result may precede final diagnostics and server-side cache
// invalidation. Always drain the stream before resolving to the caller.
export async function readClaudeTestEvents(
  body: ReadableStream<Uint8Array>,
  onEvent: (event: ClaudeTestEvent) => void,
): Promise<boolean> {
  let receivedTerminalEvent = false;
  const parser = createClaudeTestEventParser((event) => {
    if (event.type === "test_complete" || event.type === "error") receivedTerminalEvent = true;
    onEvent(event);
  });
  const reader = body.getReader();
  const decoder = new TextDecoder();
  try {
    while (true) {
      const { done, value } = await reader.read();
      if (done) break;
      parser.feed(decoder.decode(value, { stream: true }));
    }
    parser.feed(decoder.decode());
    parser.finish();
    return receivedTerminalEvent;
  } finally {
    reader.releaseLock();
  }
}

export function claudeTestTokenMetrics(usage?: ClaudeTestUsage) {
  const keys = ["input_tokens", "output_tokens", "cache_read_input_tokens", "cache_creation_input_tokens"] as const;
  const metrics = keys.map((key) => {
    const raw = usage?.[key];
    return { key, value: typeof raw === "number" && Number.isFinite(raw) && raw >= 0 ? raw : null };
  });
  const max = Math.max(1, ...metrics.map(({ value }) => value ?? 0));
  return metrics.map((item) => ({ ...item, percent: item.value === null ? 0 : item.value / max * 100 }));
}
