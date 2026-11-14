.PHONY: test build colmap install-corrallm-plugin uninstall-corrallm-plugin

# A separate module from corrallm; corrallm's go.work must not pull it in.
export GOWORK := off

# PYCOLMAP_PYTHON=<python with pycolmap 4.2.1> also checks the pose_priors blobs
# with COLMAP's own reader (skipped when unset).
test:
	go vet ./...
	go test ./...

build:
	go build -o build/colmap-svc ./cmd/colmap-svc

# COLMAP 4.2.1 + CUDA, built once into $(COLMAP_WORK)/install and reused.
COLMAP_WORK ?= $(CURDIR)/build/colmap
export COLMAP_WORK
export COLMAP_SRC COLMAP_VERSION CUDA_HOME CUDA_ARCH JOBS
colmap:
	scripts/build-colmap.sh

# corrallm plugin install. Same target name in every corrallm plugin.
CORRALLM_EXT_DIR ?= $(HOME)/.corrallm/ext
PLUGIN_NAME      := colmap
PLUGIN_DEST       = $(CORRALLM_EXT_DIR)/$(PLUGIN_NAME)
# Job state and files: outside the swapped dir, so a reinstall keeps them.
PLUGIN_DATA       = $(CORRALLM_EXT_DIR)/$(PLUGIN_NAME).data

# Builds into a temp dir beside dest, then renames. A running instance keeps
# its old (unlinked) files until corrallm respawns it. Keeps lease.token
# across reinstalls (made once, mode 600). No sudo, no corrallm reload.
install-corrallm-plugin: colmap
	@set -e; dest="$(PLUGIN_DEST)"; data="$(PLUGIN_DATA)"; tmp="$$dest.new.$$$$"; old="$$dest.old.$$$$"; \
	mkdir -p "$(CORRALLM_EXT_DIR)" "$$data"; rm -rf "$$tmp"; mkdir -p "$$tmp/bin"; \
	trap 'rm -rf "$$tmp"' EXIT; \
	go build -trimpath -o "$$tmp/bin/colmap-svc" ./cmd/colmap-svc; \
	cp -a "$(COLMAP_WORK)/install" "$$tmp/colmap"; \
	if [ -f "$$dest/lease.token" ]; then cp -p "$$dest/lease.token" "$$tmp/lease.token"; \
	else (umask 077; head -c 32 /dev/urandom | od -An -tx1 | tr -d ' \n' > "$$tmp/lease.token"); fi; \
	chmod 600 "$$tmp/lease.token"; \
	sed -e "s|@DEST@|$$dest|g" -e "s|@DATA@|$$data|g" packaging/extension.yaml.in > "$$tmp/extension.yaml"; \
	"$$tmp/colmap/bin/colmap" -h | head -1; \
	if [ -e "$$dest" ]; then mv "$$dest" "$$old"; fi; \
	mv "$$tmp" "$$dest"; trap - EXIT; rm -rf "$$old"; \
	echo "installed: $$dest (bin/colmap-svc, colmap/, extension.yaml, lease.token); state: $$data"; \
	echo "next:"; \
	echo "  1. First install only: put the lease token in the credential store (not in the config):"; \
	echo "     echo \"COLMAP_LEASE_TOKEN=\$$(cat $$dest/lease.token)\" >> ~/.corrallm/credentials; chmod 600 ~/.corrallm/credentials"; \
	echo "  2. First install only: merge $$dest/extension.yaml into the corrallm config"; \
	echo "     (corrallm config export > c.yaml; add the colmap block under extensions:; corrallm config load c.yaml)."; \
	echo "     'config load' REPLACES the whole config, so merge into a full export."; \
	echo "  3. systemctl --user reload corrallm — but extension routes mount at boot only (B3): the first"; \
	echo "     install needs a corrallm restart before /v1/files, /v1/uploads, /v1/reconstructions answer."

uninstall-corrallm-plugin:
	@rm -rf "$(PLUGIN_DEST)"; \
	echo "removed: $(PLUGIN_DEST)"; \
	echo "kept: $(PLUGIN_DATA) (files, jobs, outputs) — rm -rf it to drop them"; \
	echo "next: remove the 'colmap:' entry under extensions: in the corrallm config and COLMAP_LEASE_TOKEN"; \
	echo "  from ~/.corrallm/credentials (corrallm config export > c.yaml; edit; corrallm config load c.yaml), then"; \
	echo "  systemctl --user reload corrallm."
