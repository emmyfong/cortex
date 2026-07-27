#!/usr/bin/env node
// First-run setup: create .env from the template, verify prerequisites.
//
// Deliberately never overwrites an existing .env — that file holds real
// credentials once a developer edits it.

import { existsSync, copyFileSync } from "node:fs";
import { execSync } from "node:child_process";
import { dirname, join } from "node:path";
import { fileURLToPath } from "node:url";

const root = join(dirname(fileURLToPath(import.meta.url)), "..");
const envPath = join(root, ".env");
const examplePath = join(root, ".env.example");

let ok = true;

function check(label, command) {
  try {
    const version = execSync(command, { stdio: ["ignore", "pipe", "ignore"] })
      .toString()
      .trim()
      .split("\n")[0];
    console.log(`  ok   ${label}: ${version}`);
    return true;
  } catch {
    console.log(`  MISSING  ${label}`);
    ok = false;
    return false;
  }
}

console.log("\nChecking prerequisites:");
check("go", "go version");
check("docker", "docker --version");
check("node", "node --version");
const hasOllama = check("ollama", "ollama --version");

if (hasOllama) {
  try {
    const models = execSync("ollama list", { stdio: ["ignore", "pipe", "ignore"] }).toString();
    if (models.includes("nomic-embed-text")) {
      console.log("  ok   model nomic-embed-text");
    } else {
      console.log("  MISSING  model nomic-embed-text — run: ollama pull nomic-embed-text");
      ok = false;
    }
  } catch {
    console.log("  WARN  could not list ollama models (is the daemon running?)");
  }
}

console.log("\nEnvironment file:");
if (existsSync(envPath)) {
  console.log("  ok   .env already exists (left untouched)");
} else if (existsSync(examplePath)) {
  copyFileSync(examplePath, envPath);
  console.log("  ok   created .env from .env.example");
  console.log("       Review the credentials before running anything beyond local dev.");
} else {
  console.log("  ERROR  .env.example not found");
  ok = false;
}

if (!ok) {
  console.log("\nSetup incomplete — resolve the items above, then re-run `npm run setup`.\n");
  process.exit(1);
}

console.log("\nSetup complete. Next: npm run dev\n");
