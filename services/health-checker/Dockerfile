FROM scratch

# Be careful with the next line.
# The binary path is relative to Docker's context path set on the Makefile, not to the actual placement of this Dockerfile.
COPY bin/aetherlay-hc /aetherlay-hc

ENTRYPOINT ["/aetherlay-hc"]
