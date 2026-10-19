#!/usr/bin/env python3
"""Publish only the exported public Pix inputs through marsh's native publisher."""
import argparse
import importlib.util
import json
import os
from pathlib import Path
import subprocess

HERE = Path(__file__).resolve().parent
ROOT = HERE.parent

def main():
    parser=argparse.ArgumentParser(description=__doc__)
    parser.add_argument('--publisher',type=Path,required=True,help='path to marsh/scripts/publish-kits.py')
    parser.add_argument('--python',default='python3',help='interpreter with publisher requirements installed')
    parser.add_argument('--output',type=Path,required=True,help='new owner-private directory outside this checkout')
    parser.add_argument('--repository-prefix',default='docker.io/mcavage')
    parser.add_argument('--tag',required=True)
    parser.add_argument('--push',action='store_true',help='explicitly enable remote publication (otherwise cache-only validation)')
    args=parser.parse_args()
    output=args.output.expanduser().resolve()
    if output.exists():parser.error('output must be a new directory; do not overwrite publication evidence')
    if output.is_relative_to(ROOT) or ROOT.is_relative_to(output):parser.error('output must be outside the source tree')
    publisher=args.publisher.expanduser().resolve()
    if not publisher.is_file():parser.error('publisher script does not exist')
    output.mkdir(mode=0o700,parents=True)
    spec=importlib.util.spec_from_file_location('pix_prepare',HERE/'prepare.py')
    prepare=importlib.util.module_from_spec(spec);spec.loader.exec_module(prepare)
    snapshot=prepare.export(output/'public-source')
    additions={}
    for file in snapshot.rglob('*'):
        if file.is_file():
            relative=file.relative_to(snapshot).as_posix()
            if relative not in prepare.KIT_FILES:additions[relative]=str(file.resolve())
    inputs=output/'build-inputs.json'
    inputs.write_text(json.dumps({'marsh/pix':{'args':{},'files':additions}},indent=2)+'\n');inputs.chmod(0o600)
    registry=output/'commands.json'
    command=[args.python,str(publisher),'--source-root',str(ROOT),'--commands',str(HERE/'commands.json'),
             '--build-inputs',str(inputs),'--repository-prefix',args.repository_prefix,
             '--repository-name-prefix','marsh-','--tag',args.tag,'--output',str(registry)]
    if not args.push:command.append('--validate-only')
    subprocess.run(command,check=True)
    print('Published immutable registry: '+str(registry) if args.push else 'Native cache-only validation passed; nothing pushed.')

if __name__=='__main__':main()
