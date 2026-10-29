import assert from "node:assert/strict";
import { createRequire } from "node:module";
import { test } from "node:test";

const require = createRequire(import.meta.url);
const expoRequire = createRequire(require.resolve("expo/package.json"));
const cliRequire = createRequire(expoRequire.resolve("@expo/cli/package.json"));
const { PluginSchema, DevToolsPluginOutputSchema } = cliRequire(
  "./build/src/start/server/DevToolsPlugin.schema.js",
);
const { createMCPDevToolsExtensionSchema } = cliRequire(
  "./build/src/start/server/createMCPDevToolsExtensionSchema.js",
);

test("Expo CLI resolves the patched Zod 4 line", () => {
  const { version } = cliRequire("zod/package.json");
  const [major, minor, patch] = version.split(".").map(Number);
  assert.equal(major, 4);
  assert.ok(minor > 6 || (minor === 6 && patch >= 6), version);
});

test("Expo DevTools plugin schemas preserve validation with Zod 4", () => {
  const plugin = PluginSchema.parse({
    packageName: "fixture-plugin",
    packageRoot: "/fixture/plugin",
    cliExtensions: {
      description: "Fixture commands",
      entryPoint: "./commands.js",
      commands: [
        {
          name: "inspect",
          title: "Inspect",
          environments: ["cli", "mcp"],
          parameters: [
            { name: "label", type: "text" },
            { name: "count", type: "number" },
            { name: "confirmed", type: "confirm" },
          ],
        },
      ],
    },
  });
  const schema = createMCPDevToolsExtensionSchema(plugin);
  const command = {
    command: "inspect",
    label: "fixture",
    count: 1,
    confirmed: true,
  };

  assert.deepEqual(schema.parse(command), command);
  assert.equal(schema.safeParse({ command: "missing" }).success, false);
  assert.equal(schema.safeParse({ ...command, count: "1" }).success, false);
  assert.equal(schema.safeParse({ ...command, extra: true }).success, false);
  assert.equal(PluginSchema.safeParse({ packageName: "" }).success, false);
});

test("Expo DevTools output rejects malformed plugin responses", () => {
  const output = [{ type: "text", text: "Fixture result", level: "info" }];
  assert.deepEqual(DevToolsPluginOutputSchema.parse(output), output);
  assert.equal(
    DevToolsPluginOutputSchema.safeParse([{ type: "image", url: "not-a-url" }])
      .success,
    false,
  );
});
