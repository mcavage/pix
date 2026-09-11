import assert from 'node:assert/strict';
import fs from 'node:fs';
import os from 'node:os';
import path from 'node:path';
import {execFileSync} from 'node:child_process';
import {register} from 'node:module';
import test from 'node:test';
import {makeFakeGateway,listen,writeGatewayConfig} from './fake-mcp-gateway.mjs';
register('./stub-loader.mjs',import.meta.url);

test('recall and capture treat shell syntax in a workspace path as literal Git argv',async()=>{
 const dir=fs.mkdtempSync(path.join(os.tmpdir(),'pix-memory-path-'));const cwd=process.cwd();const prior=process.env.PI_CODING_AGENT_DIR;
 const {server,requests}=makeFakeGateway(()=>({rows:[],accepted:true}));
 try{
  const url=await listen(server);writeGatewayConfig(dir,url);process.env.PI_CODING_AGENT_DIR=dir;
  fs.mkdirSync(path.join(dir,'.pix'));fs.writeFileSync(path.join(dir,'.pix/memory-capture'),'experimental-auto');
  const repo=path.join(dir,'$(touch INJECTED) with spaces');fs.mkdirSync(repo);
  execFileSync('git',['init','-b','main',repo],{stdio:'ignore'});execFileSync('git',['-C',repo,'remote','add','origin','https://example.invalid/expected-project.git']);
  process.chdir(dir);
  for(const name of ['memory-recall','memory-capture']){
   const hooks={};const mod=await import(`../extensions/${name}.ts?literal-path`);
   mod.default({on(n,fn){hooks[n]=fn;},registerCommand(){},registerTool(){}});
   await hooks.before_agent_start({prompt:'A real user request for the current project'},{cwd:repo,sessionManager:{getBranch:()=>[{message:{role:'user',content:'A real long enough user request'}},{message:{role:'assistant',content:'Response to the user'}}]}});
   assert.equal(fs.existsSync(path.join(dir,'INJECTED')),false,`${name} executed shell syntax`);
  }
  assert.deepEqual(requests.map(r=>r.method),['memory_recall','memory_observe']);
  for(const r of requests)assert.equal(r.params.project,'expected-project');
 }finally{process.chdir(cwd);if(prior===undefined)delete process.env.PI_CODING_AGENT_DIR;else process.env.PI_CODING_AGENT_DIR=prior;await new Promise(r=>server.close(r));fs.rmSync(dir,{recursive:true,force:true});}
});
