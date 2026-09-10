import test from 'node:test';
import assert from 'node:assert/strict';
import fs from 'node:fs';
import path from 'node:path';
import os from 'node:os';
import {execFileSync} from 'node:child_process';
import {capture} from '../skills/deliver/scripts/capture-evidence.mjs';
function fixture(t) {
 const root=fs.mkdtempSync(path.join(os.tmpdir(),'pix-evidence-'));t.after(()=>fs.rmSync(root,{recursive:true,force:true}));
 const git=(...args)=>execFileSync('git',['-C',root,...args],{stdio:'pipe'});
 git('init','-b','main');git('config','user.name','Fixture');git('config','user.email','fixture@example.invalid');
 fs.writeFileSync(path.join(root,'.gitignore'),'.pi-agent/\nignored.txt\n');fs.writeFileSync(path.join(root,'app.txt'),'original');git('add','.');git('-c','commit.gpgsign=false','commit','-m','baseline');
 fs.mkdirSync(path.join(root,'.pi-agent'));fs.writeFileSync(path.join(root,'.pi-agent/contract.md'),'C1: preserve input');
 const spec={contract:'.pi-agent/contract.md',criteria:['C1'],environment:'fixture node',command:[process.execPath,'-e','console.log("actual output")']};
 const run=()=>{const file=path.join(root,'.pi-agent/spec.json');fs.writeFileSync(file,JSON.stringify(spec));return capture(file,'.pi-agent/check-1',root);};
 return {root,spec,run};
}
test('real command logs and identity captured without claiming acceptance; cannot overwrite',t=>{
 const f=fixture(t),r=f.run();assert.equal(r.commandPassed,true);assert.equal(r.accepted,false);assert.equal(r.stable,true);
 assert.equal(fs.readFileSync(path.join(f.root,'.pi-agent/check-1/stdout.log'),'utf8'),'actual output\n');assert.throws(f.run,/EEXIST/);
});
test('failed command preserves exit and output, never passes',t=>{
 const f=fixture(t);f.spec.command=[process.execPath,'-e','console.error("failure evidence");process.exit(7)'];const r=f.run();assert.equal(r.exit,7);assert.equal(r.commandPassed,false);assert.match(fs.readFileSync(path.join(f.root,'.pi-agent/check-1/stderr.log'),'utf8'),/failure evidence/);
});
for(const file of ['app.txt','new-test.txt','ignored.txt','.pi-agent/contract.md'])test(`exit zero cannot validate changed ${file}`,t=>{
 const f=fixture(t);f.spec.ignoredShipping=['ignored.txt'];f.spec.command=[process.execPath,'-e',`require('fs').writeFileSync(${JSON.stringify(file)},'changed')`];const r=f.run();assert.equal(r.exit,0);assert.equal(r.stable,false);assert.equal(r.commandPassed,false);
});
test('missing executable preserves an incomplete record',t=>{
 const f=fixture(t);f.spec.command=['/definitely-not-a-command'];const r=f.run();assert.equal(r.commandPassed,false);assert.match(r.executionError,/ENOENT/);
});
test('symlinked output parent cannot create directories outside repository',t=>{
 const f=fixture(t),outside=fs.mkdtempSync(path.join(os.tmpdir(),'pix-evidence-outside-'));t.after(()=>fs.rmSync(outside,{recursive:true,force:true}));
 fs.symlinkSync(outside,path.join(f.root,'.pi-agent/link'));const file=path.join(f.root,'.pi-agent/spec.json');fs.writeFileSync(file,JSON.stringify(f.spec));
 assert.throws(()=>capture(file,'.pi-agent/link/nested/check',f.root),/escapes/);assert.deepEqual(fs.readdirSync(outside),[]);
});

test('deleted contract after command leaves retained incomplete evidence',t=>{
 const f=fixture(t);f.spec.command=[process.execPath,'-e',"require('fs').unlinkSync('.pi-agent/contract.md')"];const r=f.run();assert.equal(r.commandPassed,false);assert.equal(r.after,null);assert.match(r.identityError,/ENOENT/);
});
