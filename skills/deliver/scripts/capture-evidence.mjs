// Record real execution and candidate identity; never a product-acceptance oracle.
import fs from 'node:fs';
import path from 'node:path';
import crypto from 'node:crypto';
import {execFileSync, spawnSync} from 'node:child_process';
import {fileURLToPath} from 'node:url';
import {source} from './repair-packet.mjs';
const hash = bytes => crypto.createHash('sha256').update(bytes).digest('hex');
function ensure(ok, message) { if (!ok) throw Error(message); }
function input(root, name) {
 ensure(typeof name === 'string' && !path.isAbsolute(name), 'Inputs must be repository relative');
 const file = path.resolve(root, name);
 ensure(file.startsWith(root + path.sep) && fs.realpathSync(file).startsWith(root + path.sep), 'Input escapes repository');
 ensure(fs.statSync(file).isFile(), 'Input must be a file');
 return {path: name, sha256: hash(fs.readFileSync(file))};
}
export function capture(specFile, output, cwd = process.cwd()) {
 const root = fs.realpathSync(execFileSync('git', ['-C', cwd, 'rev-parse', '--show-toplevel'], {encoding:'utf8'}).trim());
 ensure(fs.statSync(specFile).size <= 12 * 1024, 'Spec exceeds 12 KiB');
 const spec = JSON.parse(fs.readFileSync(specFile, 'utf8'));
 ensure(Array.isArray(spec.command) && spec.command.length > 0 && spec.command.every(x => typeof x === 'string' && !x.includes('\0')), 'Supply command argv, not a shell string');
 ensure(Array.isArray(spec.criteria) && spec.criteria.length > 0 && spec.criteria.every(x => typeof x === 'string' && x.trim()), 'Supply criterion IDs');
 ensure(typeof spec.environment === 'string' && spec.environment.trim(), 'Describe relevant toolchain/profile without secrets');
 const ignoredShipping = spec.ignoredShipping ?? [];
 ensure(Array.isArray(ignoredShipping) && ignoredShipping.every(x => typeof x === 'string' && !x.startsWith('.pi-agent/')), 'Invalid ignored shipping inventory');
 const inputs = () => ({contract: input(root, spec.contract), inputs: (spec.inputs ?? []).map(n => input(root,n))});
 const before = {candidate: source(root, ignoredShipping, true), ...inputs()};
 const target = path.resolve(root, output);
 ensure(target.startsWith(path.join(root, '.pi-agent') + path.sep), 'Output must be inside .pi-agent/');
 // Check ancestors before mkdir: an existing symlink must never create outside dirs.
 let ancestor = path.dirname(target);
 while (!fs.existsSync(ancestor)) ancestor = path.dirname(ancestor);
 ensure(fs.realpathSync(ancestor) === root || fs.realpathSync(ancestor).startsWith(root + path.sep), 'Output parent escapes repository');
 fs.mkdirSync(path.dirname(target), {recursive:true});
 ensure(fs.realpathSync(path.dirname(target)).startsWith(path.join(root,'.pi-agent') + path.sep) || fs.realpathSync(path.dirname(target)) === path.join(root,'.pi-agent'), 'Output parent escapes .pi-agent/');
 fs.mkdirSync(target, {mode:0o700}); // No overwrite of a previous run.
 const stdout = fs.openSync(path.join(target,'stdout.log'),'wx',0o600);
 const stderr = fs.openSync(path.join(target,'stderr.log'),'wx',0o600);
 const startedAt = new Date().toISOString();
 fs.writeFileSync(path.join(target,'started.json'),JSON.stringify({root, command:spec.command, criteria:spec.criteria, environment:spec.environment, before, startedAt, completed:false},null,2)+'\n',{flag:'wx',mode:0o600});
 let execution;
 try { execution = spawnSync(spec.command[0], spec.command.slice(1), {cwd:root,stdio:['ignore',stdout,stderr]}); }
 finally { fs.closeSync(stdout); fs.closeSync(stderr); }
 const endedAt = new Date().toISOString();
 let after, identityError;
 try { after = {candidate: source(root, ignoredShipping, true), ...inputs()}; }
 catch (error) { identityError = error.message; }
 const stable = !!after && JSON.stringify(before) === JSON.stringify(after);
 const record = {version:1, root, criteria:spec.criteria, command:spec.command, environment:spec.environment,
  ignoredShipping, startedAt, endedAt, exit:execution.status, signal:execution.signal,
  executionError:execution.error?.message ?? null, before, after:after ?? null, identityError:identityError ?? null,
  stable, commandPassed:execution.status === 0 && !execution.error && stable, accepted:false,
  logs:{stdout:'stdout.log',stderr:'stderr.log'},
  meaning:'Command exit and input stability only. Inspect logs and user-visible behavior; independent review and acceptance remain separate.'};
 fs.writeFileSync(path.join(target,'execution.json'),JSON.stringify(record,null,2)+'\n',{flag:'wx',mode:0o600});
 return record;
}
if (process.argv[1] && path.resolve(process.argv[1]) === fileURLToPath(import.meta.url)) {
 try {
  const [spec, output, ...extra] = process.argv.slice(2);
  ensure(spec && output && !extra.length, 'Usage: capture-evidence.mjs SPEC .pi-agent/deliver/SLUG/checks/UNIQUE-RUN');
  const r = capture(spec, output);
  console.log(JSON.stringify({record:path.join(output,'execution.json'),commandPassed:r.commandPassed,stable:r.stable,accepted:false}));
  if (!r.commandPassed) process.exitCode = 1;
 } catch (error) { console.error(`Evidence not recorded: ${error.message}`); process.exitCode = 1; }
}
