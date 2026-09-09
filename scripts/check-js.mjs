import {readdirSync} from "node:fs";
import {spawnSync} from "node:child_process";
import {join} from "node:path";
import process from "node:process";

const staticDirectory = join(process.cwd(), "app", "static");
const files = readdirSync(staticDirectory)
    .filter((file) => file.endsWith(".js"))
    .sort();

for (const file of files) {
    const path = join(staticDirectory, file);
    const result = spawnSync(process.execPath, ["--check", path], {
        stdio: "inherit",
    });
    if (result.status !== 0) {
        process.exit(result.status ?? 1);
    }
}

console.log(`Syntax checked: ${files.length} JavaScript modules.`);
