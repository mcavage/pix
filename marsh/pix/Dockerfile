ARG PI_BASE=docker.io/mcavage/marsh-pi:latest
FROM ${PI_BASE}
ARG PI_BASE
# A published build names its base by digest; marsh/kit_inputs.py compares it.
LABEL org.opencontainers.image.base.name="${PI_BASE}"
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
      --receipt /usr/local/share/licenses/pix/public-check.json \
 && rm -rf /usr/share/sandbox/kit/pi
# A kit set reads its components' declarations from /usr/share/sandbox/kit;
# keep only Pix's own there, not the inherited Pi Kit's, so a set can list Pix.
# Pi expands this tilde after HOME is selected. An unchanged image default is
# not implicitly forwarded by marsh when Pix launches a different Kit.
ENV PI_CODING_AGENT_DIR="~/.pix-agent/agent" PIX_PACKAGE_DIR=/opt/pix/package
USER agent
ENTRYPOINT ["/usr/local/bin/marsh-pix"]
