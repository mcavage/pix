#!/usr/bin/env python3
"""Publish only the exported public Pix inputs through marsh's native publisher."""
import argparse
import importlib.util
import json
from pathlib import Path
import subprocess

HERE = Path(__file__).resolve().parent
ROOT = HERE.parent

def load(name):
    spec=importlib.util.spec_from_file_location('pix_'+name,HERE/f'{name}.py')
    module=importlib.util.module_from_spec(spec);spec.loader.exec_module(module)
    return module

def main():
    parser=argparse.ArgumentParser(description=__doc__)
    parser.add_argument('--publisher',type=Path,required=True,help='path to marsh/scripts/publish-kits.py')
    parser.add_argument('--python',default='python3',help='interpreter with publisher requirements installed')
    parser.add_argument('--output',type=Path,required=True,help='new owner-private directory outside this checkout')
    parser.add_argument('--repository-prefix',default='docker.io/mcavage')
    parser.add_argument('--base',default='docker.io/mcavage/marsh-pi:latest',
                        help="marsh's Pi image; a tag is resolved to its current digest")
    parser.add_argument('--expect-version',help='refuse to publish any other Kit version')
    parser.add_argument('--tag',required=True)
    parser.add_argument('--push',action='store_true',help='explicitly enable remote publication (otherwise cache-only validation)')
    args=parser.parse_args()
    output=args.output.expanduser().resolve()
    if output.exists():parser.error('output must be a new directory; do not overwrite publication evidence')
    if output.is_relative_to(ROOT) or ROOT.is_relative_to(output):parser.error('output must be outside the source tree')
    publisher=args.publisher.expanduser().resolve()
    if not publisher.is_file():parser.error('publisher script does not exist')
    prepare,kit_inputs=load('prepare'),load('kit_inputs')
    base=kit_inputs.resolve_base(args.base)
    output.mkdir(mode=0o700,parents=True)
    # The publisher builds a Git source tree. Give it the exported snapshot, with
    # the Kit version stamped into its descriptor, rather than this checkout.
    source=output/'source'
    snapshot=prepare.export(source/'pix')
    arguments,version=kit_inputs.stamp(snapshot,base)
    if args.expect_version and version!=args.expect_version:
        parser.error(f'Kit version is {version}, expected {args.expect_version}')
    subprocess.run(['git','init','-q',str(source)],check=True)
    subprocess.run(['git','-C',str(source),'add','-A'],check=True)
    commands=output/'kit-commands.json'
    commands.write_text(json.dumps({'pix':'pix'})+'\n');commands.chmod(0o600)
    inputs=output/'build-inputs.json'
    inputs.write_text(json.dumps({'pix':{'args':arguments,'files':{}}},indent=2)+'\n');inputs.chmod(0o600)
    (output/'kit.json').write_text(json.dumps({'version':version,**arguments},indent=2)+'\n')
    print(f'Pix Kit {version} on {arguments["base"]}',flush=True)
    registry=output/'commands.json'
    command=[args.python,str(publisher),'--source-root',str(source),'--commands',str(commands),
             '--build-inputs',str(inputs),'--repository-prefix',args.repository_prefix,
             '--repository-name-prefix','marsh-','--tag',args.tag,'--output',str(registry)]
    if not args.push:command.append('--validate-only')
    subprocess.run(command,check=True)
    print('Published immutable registry: '+str(registry) if args.push else 'Native cache-only validation passed; nothing pushed.')

if __name__=='__main__':main()
