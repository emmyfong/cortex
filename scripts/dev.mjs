#!/usr/bin/env node
// Runs api, worker, and web together with prefixed output.
//
// Avoids a `concurrently` dependency: this is the only thing we needed it for,
// and child_process handles it in a few lines.

import { spawn } from "node:child_process";
import { dirname, join } from "node:path";
import { fileURLToPath } from "node:url";

const root = join(dirname(fileURLToPath(import.meta.url)), "..");

const services = [
  { name: "api", color: "\x1b[36m", command: "go", args: ["run", "./cmd/api"], cwd: join(root, "backend") },
  { name: "worker", color: "\x1b[35m", command: "go", args: ["run", "./cmd/worker"], cwd: join(root, "backend") },
  { name: "web", color: "\x1b[32m", command: "npm", args: ["run", "dev"], cwd: join(root, "web") },
];

const RESET = "\x1b[0m";
const children = [];
let shuttingDown = false;

function prefix(name, color, stream) {
  let buffer = "";
  stream.on("data", (chunk) => {
    buffer += chunk.toString();
    const lines = buffer.split("\n");
    buffer = lines.pop() ?? "";
    for (const line of lines) {
      console.log(`${color}[${name}]${RESET} ${line}`);
    }
  });
}

for (const service of services) {
  const child = spawn(service.command, service.args, {
    cwd: service.cwd,
    shell: true,
    stdio: ["ignore", "pipe", "pipe"],
  });

  prefix(service.name, service.color, child.stdout);
  prefix(service.name, service.color, child.stderr);

  child.on("exit", (code) => {
    if (shuttingDown) return;
    console.log(`${service.color}[${service.name}]${RESET} exited with code ${code}`);
    shutdown(code ?? 1);
  });

  children.push(child);
}

function shutdown(code) {
  if (shuttingDown) return;
  shuttingDown = true;
  for (const child of children) {
    if (!child.killed) child.kill("SIGTERM");
  }
  process.exit(code);
}

process.on("SIGINT", () => shutdown(0));
process.on("SIGTERM", () => shutdown(0));

console.log("Running api, worker, and web. Ctrl+C to stop all three.\n");
