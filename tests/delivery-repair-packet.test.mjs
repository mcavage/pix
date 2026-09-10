import test from 'node:test';
import assert from 'node:assert/strict';
import fs from 'node:fs';
import os from 'node:os';
import path from 'node:path';
import {execFileSync, spawnSync} from 'node:child_process';
import {fileURLToPath, pathToFileURL} from 'node:url';
import {register} from 'node:module';
register('./stub-loader.mjs', import.meta.url);
const script = fileURLToPath(new URL('../skills/deliver/scripts/repair-packet.mjs', import.meta.url));
function fixture(t) {
 const root=fs.mkdtempSync(path.join(os.tmpdir(),'pix-repair-'));t.after(()=>fs.rmSync(root,{recursive:true,force:true}));
 const git=(...args)=>execFileSync('git',['-C',root,...args],{stdio:'pipe'});
 git('init','-q');git('config','user.name','Fixture');git('config','user.email','fixture@example.invalid');
 fs.mkdirSync(path.join(root,'.pi-agent/deliver'),{recursive:true});
 const write=(name,content)=>fs.writeFileSync(path.join(root,name),content);
 write('.gitignore','.pi-agent/\nignored.txt\n');write('drafts.mjs','export const key = (form, entity) => form;\n');
 git('add','.');git('-c','commit.gpgsign=false','commit','-qm','Synthetic baseline');
 const refs={};for(const name of ['contract','review','candidate','evidence']){refs[name]=`.pi-agent/deliver/${name}.json`;write(refs[name],JSON.stringify({kind:name,criteria:['record isolation'],verdict:name==='review'?'BLOCK':null}));}
 const spec={task:'Fix record identity without changing the product contract.',findings:[{id:'F-1',text:'Unsaved form edits cross entity boundaries in drafts.mjs.'}],references:refs};
 const specPath='.pi-agent/deliver/spec.json',packet='.pi-agent/deliver/packet.json';write(specPath,JSON.stringify(spec));
 const cli=(...args)=>spawnSync(process.execPath,[script,...args],{cwd:root,encoding:'utf8'});
 const create=()=>{const r=cli('create',specPath,packet);assert.equal(r.status,0,r.stderr);return JSON.parse(r.stdout);};
 return {root,write,git,spec,specPath,packet,cli,create};
}
test('fresh process repairs a real regression; result still requires verification and review',t=>{
 const f=fixture(t);f.create();
 const fresh=spawnSync(process.execPath,['--input-type=module','-e',`
 import fs from 'node:fs';import {execFileSync} from 'node:child_process';
 const p=JSON.parse(execFileSync(process.execPath,[${JSON.stringify(script)},'check',${JSON.stringify(f.packet)}],{encoding:'utf8'}));
 if(p.findings[0].id!=='F-1')throw Error('Finding lost');
 fs.writeFileSync('drafts.mjs','export const key = (form, entity) => JSON.stringify([form, entity]);\\n');
 `],{cwd:f.root,encoding:'utf8'});assert.equal(fresh.status,0,fresh.stderr);
 const r=f.cli('result',f.packet,'.pi-agent/deliver/result.json');assert.equal(r.status,0,r.stderr);const out=JSON.parse(r.stdout);assert.deepEqual(out.findings,['F-1']);assert.equal(out.state,'needs-verification-and-review');assert.equal(out.accepted,false);
 const oracle="import {key} from './drafts.mjs';import assert from 'node:assert/strict';const drafts=new Map();drafts.set(key('plan','A'),'A-only');assert.equal(drafts.get(key('plan','B')),undefined);";
 assert.equal(spawnSync(process.execPath,['--input-type=module','-e',oracle],{cwd:f.root}).status,0);
 f.write('drafts.mjs','export const key = (form, entity) => form;\n');assert.notEqual(spawnSync(process.execPath,['--input-type=module','-e',oracle],{cwd:f.root}).status,0,'negative control must reject original bug');
});
test('repeated reads are no progress; never overwrite prior attempt evidence',t=>{
 const f=fixture(t);f.create();for(let i=0;i<4;i++)assert.equal(f.cli('check',f.packet).status,0);
 const r=f.cli('result',f.packet,'.pi-agent/deliver/result.json');assert.equal(r.status,2);assert.equal(JSON.parse(r.stdout).state,'blocked-no-source-progress');assert.equal(JSON.parse(r.stdout).accepted,false);assert.notEqual(f.cli('result',f.packet,'.pi-agent/deliver/result.json').status,0);
});
for(const mutation of ['tracked','untracked','ignored','deleted','staged','contract','review','evidence'])test(`fresh worker rejects stale ${mutation} input`,t=>{
 const f=fixture(t);if(mutation==='ignored'){f.spec.ignoredShipping=['ignored.txt'];f.write('ignored.txt','before');f.write(f.specPath,JSON.stringify(f.spec));}f.create();
 if(mutation==='tracked')f.write('drafts.mjs','changed');else if(mutation==='untracked')f.write('new-test.mjs','new');else if(mutation==='ignored')f.write('ignored.txt','changed');else if(mutation==='deleted')fs.unlinkSync(path.join(f.root,'drafts.mjs'));else if(mutation==='staged'){f.write('drafts.mjs','changed');f.git('add','drafts.mjs');f.write('drafts.mjs','export const key = (form, entity) => form;\n');}else f.write(f.spec.references[mutation],'changed');
 const r=f.cli('check',f.packet);assert.equal(r.status,1);assert.match(r.stderr,/Stale/);
});
test('large evidence stays behind references; oversized findings fail without truncation',t=>{
 const f=fixture(t);f.write(f.spec.references.evidence,'large raw trace\n'.repeat(100000));assert.ok(f.create().bytes<4096);const r=f.cli('check',f.packet);assert.ok(r.stdout.length<4096);assert.doesNotMatch(r.stdout,/large raw trace/);f.spec.findings[0].text='x'.repeat(1100);f.write(f.specPath,JSON.stringify(f.spec));assert.equal(f.cli('create',f.specPath,'.pi-agent/deliver/oversize.json').status,1);assert.equal(fs.existsSync(path.join(f.root,'.pi-agent/deliver/oversize.json')),false);
});
test('reference and output symlinks cannot escape repository',t=>{
 const f=fixture(t),outside=fs.mkdtempSync(path.join(os.tmpdir(),'pix-outside-'));t.after(()=>fs.rmSync(outside,{recursive:true,force:true}));fs.writeFileSync(path.join(outside,'private'),'fixture');fs.symlinkSync(path.join(outside,'private'),path.join(f.root,'external'));f.spec.references.review='external';f.write(f.specPath,JSON.stringify(f.spec));assert.equal(f.cli('create',f.specPath,f.packet).status,1);fs.unlinkSync(path.join(f.root,'external'));f.spec.references.review='.pi-agent/deliver/review.json';f.write(f.specPath,JSON.stringify(f.spec));fs.symlinkSync(outside,path.join(f.root,'.pi-agent','escape'));assert.equal(f.cli('create',f.specPath,'.pi-agent/escape/packet.json').status,1);assert.equal(fs.existsSync(path.join(outside,'packet.json')),false);
});
test('new scratch logs cannot turn a stalled repair into source progress',t=>{
 const f=fixture(t);f.create();f.write('.pi-agent/deliver/new-log.txt','Reread everything again');
 const r=f.cli('result',f.packet,'.pi-agent/deliver/result.json');assert.equal(r.status,2);assert.equal(JSON.parse(r.stdout).state,'blocked-no-source-progress');
});
test('repair cannot overwrite the original review evidence and report completion',t=>{
 const f=fixture(t);f.create();f.write('drafts.mjs','export const key = (form, entity) => JSON.stringify([form, entity]);\n');f.write(f.spec.references.review,'LGTM');
 const r=f.cli('result',f.packet,'.pi-agent/deliver/result.json');assert.equal(r.status,1);assert.match(r.stderr,/Stale review/);assert.equal(fs.existsSync(path.join(f.root,'.pi-agent/deliver/result.json')),false);
});
test('staging or committing existing bytes is not repair progress',t=>{
 const f=fixture(t);f.write('drafts.mjs','export const key = (form, entity) => form; // pre-existing edit\n');f.create();f.git('add','drafts.mjs');f.git('-c','commit.gpgsign=false','commit','-qm','Existing bytes only');
 assert.equal(f.cli('check',f.packet).status,1,'candidate identity changed');
 const r=f.cli('result',f.packet,'.pi-agent/deliver/result.json');assert.equal(r.status,2);assert.equal(JSON.parse(r.stdout).state,'blocked-no-source-progress');
});
test('dispatch uses the existing two-step chain with fresh path-only review context',t=>{
 const f=fixture(t);f.create();const r=f.cli('dispatch',f.packet);assert.equal(r.status,0,r.stderr);const args=JSON.parse(r.stdout);assert.deepEqual(args.chain.map(s=>s.agent),['engineer','review']);assert.ok(args.chain.every(s=>s.cwd===fs.realpathSync(f.root)));assert.ok(r.stdout.length<6000);
 assert.ok(args.chain[0].task.includes('check'));assert.ok(args.chain[0].task.includes('result'));assert.ok(args.chain[1].task.includes(f.packet+'.result.json'));assert.doesNotMatch(args.chain[1].task,/\{previous\}/);assert.ok(args.chain[1].task.includes('secretScan'));assert.ok(args.chain[1].task.includes('BLOCK'));
 f.write('drafts.mjs','changed');assert.equal(f.cli('dispatch',f.packet).status,1,'never launch against stale inputs');
});
test('generated handoff runs repair then verification through the real subagent chain caller',async t=>{
 const f=fixture(t),saved={...process.env};
 try {
  const agentDir=path.join(f.root,'.pi-agent','runner');fs.mkdirSync(path.join(agentDir,'agents'),{recursive:true});
  for(const name of ['engineer','review'])fs.writeFileSync(path.join(agentDir,'agents',name+'.md'),`---\ndescription: scripted fixture\ntools: read\nweb: false\nmodel: fixture/${name}\n---\nFixture only.\n`);
  f.create();const params=JSON.parse(f.cli('dispatch',f.packet).stdout);
  const packet=path.join(f.root,f.packet),child=path.join(agentDir,'child.mjs');
  fs.writeFileSync(child,`
   import fs from 'node:fs';import assert from 'node:assert/strict';import {execFileSync} from 'node:child_process';
   const packet=${JSON.stringify(packet)},script=${JSON.stringify(script)};
   const engineer=process.argv.includes('fixture/engineer');
   if(engineer){
    execFileSync(process.execPath,[script,'check',packet]);
    fs.writeFileSync('drafts.mjs','export const key = (form, entity) => JSON.stringify([form, entity]);\\n');
    execFileSync(process.execPath,[script,'result',packet,packet+'.result.json']);
   } else {
    const result=JSON.parse(fs.readFileSync(packet+'.result.json'));
    assert.equal(result.state,'needs-verification-and-review');assert.equal(result.accepted,false);
    const {key}=await import(${JSON.stringify(pathToFileURL(path.join(f.root,'drafts.mjs')).href)});
    const drafts=new Map();drafts.set(key('plan','A'),'A-only');assert.equal(drafts.get(key('plan','B')),undefined);
    assert(!process.argv.at(-1).includes('parent-transcript-sentinel'));
   }
   console.log(JSON.stringify({type:'message_end',message:{role:'assistant',provider:engineer?'fixture-author':'fixture-reviewer',model:'scripted',content:[{type:'text',text:engineer?'parent-transcript-sentinel':'LGTM: fixture record isolation verified'}],stopReason:'stop'}}));
  `);
  process.env.PI_TEST_AGENT_DIR=agentDir;process.env.PI_SUBAGENT_PI_COMMAND=`${process.execPath} ${child}`;delete process.env.PI_SUBAGENT_DISABLED;delete process.env.PI_SUBAGENT_DEPTH;
  const mod=await import('../extensions/subagents.ts?repair-chain');let tool;mod.default({on(){},registerCommand(){},registerTool(t){tool=t;}});
  const result=await tool.execute('repair-fixture',params,new AbortController().signal,()=>{},{cwd:f.root});
  assert.equal(result.isError,undefined,JSON.stringify(result));assert.equal(result.details.results.length,2);assert.match(result.content[0].text,/LGTM/);
  assert.equal(result.details.results[0].messages.at(-1).provider,'fixture-author');assert.equal(result.details.results[1].messages.at(-1).provider,'fixture-reviewer');
 } finally {for(const key of Object.keys(process.env))if(!(key in saved))delete process.env[key];Object.assign(process.env,saved);}
});
