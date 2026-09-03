import { describe, expect, it } from "vitest";
import { parseSSE } from "./api";

describe("parseSSE", () => {
  it("parses a single complete frame", () => {
    const [frames, remainder] = parseSSE('id: 1\ndata: {"a":1}\n\n');
    expect(frames).toEqual([{ id: "1", data: '{"a":1}' }]);
    expect(remainder).toBe("");
  });

  it("holds back a trailing partial frame", () => {
    const [frames, remainder] = parseSSE('id: 1\ndata: {"a":1}\n\nid: 2\ndata: {"b"');
    expect(frames).toEqual([{ id: "1", data: '{"a":1}' }]);
    expect(remainder).toBe('id: 2\ndata: {"b"');
  });

  it("resumes a frame split across chunk boundaries", () => {
    const [first, remainder] = parseSSE("id: 1\ndata: {\"a\"");
    expect(first).toEqual([]);

    const [second, rest] = parseSSE(remainder + ':1}\n\n');
    expect(second).toEqual([{ id: "1", data: '{"a":1}' }]);
    expect(rest).toBe("");
  });

  it("joins a multi-line data payload", () => {
    const [frames] = parseSSE("id: 1\ndata: line one\ndata: line two\n\n");
    expect(frames).toEqual([{ id: "1", data: "line one\nline two" }]);
  });

  it("skips blank keep-alive frames", () => {
    const [frames, remainder] = parseSSE("\n\nid: 1\ndata: x\n\n");
    expect(frames).toEqual([{ id: "1", data: "x" }]);
    expect(remainder).toBe("");
  });
});
