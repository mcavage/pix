FROM docker.io/mcavage/marsh-pi@sha256:02f99d938eb27cf0b484cb2577373e91ec16061734e76baad6944c5affe5a4f6
COPY --chmod=0755 entrypoint.sh /usr/local/bin/marsh-pix
COPY package /opt/pix/package
COPY --chmod=0644 LICENSE /usr/local/share/licenses/pix/LICENSE
COPY --chmod=0644 PUBLIC-SOURCES.json /usr/local/share/licenses/pix/inputs.json
COPY --chmod=0644 verify-public.py /usr/local/share/licenses/pix/verify-public.py
USER root
# Recheck the actual derivative, not just the inherited base's old receipt.
RUN chmod 0755 /usr/local/share/licenses/pix \
 && python3 /usr/local/share/licenses/marsh-dhi/verify.py --root / --adapter pi \
      --receipt-dir /usr/local/share/licenses/pix/base-check >/dev/null \
 && python3 /usr/local/share/licenses/pix/verify-public.py \
      --receipt /usr/local/share/licenses/pix/public-check.json
# Pi expands this tilde after HOME is selected. An unchanged image default is
# not implicitly forwarded by marsh when Pix launches a different Kit.
ENV PI_CODING_AGENT_DIR="~/.pix-agent/agent" PIX_PACKAGE_DIR=/opt/pix/package
USER agent
ENTRYPOINT ["/usr/local/bin/marsh-pix"]
