ARG PIX_AGENT_IMAGE=docker.io/mcavage/pix-agent:0.1.100
FROM ${PIX_AGENT_IMAGE}
ENV NODE_EXTRA_CA_CERTS=/usr/local/share/ca-certificates/proxy-ca.crt
ENTRYPOINT ["pi", "--session-dir", ".pi-sessions", "--models", "anthropic/claude-opus-5-5,anthropic/claude-sonnet-5-5,openai/gpt-6-astra,openai/gpt-6.1-sol,openai/gpt-6-luna,google/gemini-3.1-pro-preview,google/gemini-3.8-flash,ollama/*"]
CMD []
