import { describe, expect, it } from "vitest";
import { parseWorkspaceConfig } from "../src/config";
import type { Env } from "../src/types";

function cfg(overrides: Partial<Env> = {}) {
  return parseWorkspaceConfig(overrides as Env);
}

describe("workspace configuration", () => {
  it("uses private zero-config defaults", () => {
    expect(cfg()).toMatchObject({
      name: "abbs",
      visibility: "private",
      directoryListing: false,
      authMode: "first-claim",
    });
  });

  it.each([
    ["name too long", { WORKSPACE_NAME: "界".repeat(101) }],
    ["description too long", { WORKSPACE_DESCRIPTION: "界".repeat(1001) }],
    ["visibility", { WORKSPACE_VISIBILITY: "internet" }],
    ["auth", { AUTH_MODE: "magic" }],
    ["public missing canonical", { WORKSPACE_VISIBILITY: "public" }],
    ["private listing", { WORKSPACE_DIRECTORY_LISTING: "true" }],
    [
      "listed missing description",
      {
        WORKSPACE_VISIBILITY: "public",
        WORKSPACE_CANONICAL_URL: "https://example.com",
        WORKSPACE_DIRECTORY_LISTING: "true",
      },
    ],
    ["listing boolean", { WORKSPACE_DIRECTORY_LISTING: "yes" }],
    ["canonical http", { WORKSPACE_CANONICAL_URL: "http://example.com" }],
    ["canonical credentials", { WORKSPACE_CANONICAL_URL: "https://user@example.com" }],
    ["canonical path", { WORKSPACE_CANONICAL_URL: "https://example.com/abbs" }],
    ["canonical query", { WORKSPACE_CANONICAL_URL: "https://example.com?x=1" }],
    ["canonical fragment", { WORKSPACE_CANONICAL_URL: "https://example.com#x" }],
    ["canonical whitespace", { WORKSPACE_CANONICAL_URL: " https://example.com" }],
    ["canonical uppercase scheme", { WORKSPACE_CANONICAL_URL: "HTTPS://example.com" }],
  ])("rejects invalid %s", (_name, bindings) => {
    expect(() => cfg(bindings)).toThrow();
  });

  it("preserves valid presentation text and publication metadata", () => {
    expect(
      cfg({
        WORKSPACE_NAME: "公開",
        WORKSPACE_DESCRIPTION: "plain *text*",
        WORKSPACE_VISIBILITY: "public",
        WORKSPACE_CANONICAL_URL: "https://example.com/",
        WORKSPACE_DIRECTORY_LISTING: "true",
      }),
    ).toEqual({
      name: "公開",
      description: "plain *text*",
      visibility: "public",
      canonicalUrl: "https://example.com/",
      directoryListing: true,
      authMode: "first-claim",
    });
  });

  it("allows an optional valid canonical origin on private workspaces", () => {
    expect(cfg({ WORKSPACE_CANONICAL_URL: "https://private.example" }).canonicalUrl).toBe(
      "https://private.example",
    );
  });
});
