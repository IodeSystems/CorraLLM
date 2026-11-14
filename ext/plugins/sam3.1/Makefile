.PHONY: test install-corrallm-plugin uninstall-corrallm-plugin

# Dev venv (editable shim + test deps), built on first use.
.venv:
	scripts/venv.sh

test: .venv
	CUDA_VISIBLE_DEVICES= .venv/bin/python -m pytest

# corrallm plugin install. Same target name in every corrallm plugin.
CORRALLM_EXT_DIR ?= $(HOME)/.corrallm/ext
PLUGIN_NAME      := sam3.1
PLUGIN_DEST       = $(CORRALLM_EXT_DIR)/$(PLUGIN_NAME)

# Builds the venv in a temp dir beside dest, then renames. A running instance keeps its
# old (unlinked) files until corrallm respawns it. Re-run = full rebuild + swap.
# No sudo, no weights download, no corrallm reload. ~8 GB of wheels (uv cache helps).
# The venv is built at the temp path and renamed: only `python -m` is safe afterwards
# (entry-point script shebangs keep the temp path). The cmd uses `python -m`.
install-corrallm-plugin:
	@set -e; dest="$(PLUGIN_DEST)"; tmp="$$dest.new.$$$$"; old="$$dest.old.$$$$"; \
	mkdir -p "$(CORRALLM_EXT_DIR)"; rm -rf "$$tmp"; mkdir -p "$$tmp"; \
	trap 'rm -rf "$$tmp"' EXIT; \
	VENV="$$tmp/venv" SHIM_SPEC="." scripts/venv.sh; \
	VENV="$$tmp/venv" SAM3_INSTALL=wheel scripts/venv-sam3.sh; \
	sed "s|@DEST@|$$dest|g" packaging/extension.yaml.in > "$$tmp/extension.yaml"; \
	if [ -e "$$dest" ]; then mv "$$dest" "$$old"; fi; \
	mv "$$tmp" "$$dest"; trap - EXIT; rm -rf "$$old"; \
	echo "installed: $$dest (venv/, extension.yaml)"; \
	echo "next:"; \
	echo "  1. Weights (not downloaded here; gated, accept the licence on huggingface.co/facebook/sam3.1):"; \
	echo "     huggingface-cli download facebook/sam3.1 sam3.1_multiplex.pt --local-dir ~/.corrallm/models/sam3.1"; \
	echo "     -> ~/.corrallm/models/sam3.1/sam3.1_multiplex.pt (3.5 GB)"; \
	echo "  2. First install only: merge $$dest/extension.yaml into the corrallm config"; \
	echo "     (corrallm config export > c.yaml; add/replace the sam3 block under extensions:; corrallm config load c.yaml)."; \
	echo "     'config load' REPLACES the whole config, so merge into a full export."; \
	echo "  3. systemctl --user reload corrallm"

uninstall-corrallm-plugin:
	@rm -rf "$(PLUGIN_DEST)"; \
	echo "removed: $(PLUGIN_DEST)"; \
	echo "next: remove the 'sam3:' entry under extensions: in the corrallm config"; \
	echo "  (corrallm config export > c.yaml; edit; corrallm config load c.yaml), then"; \
	echo "  systemctl --user reload corrallm. Weights in ~/.corrallm/models/sam3.1 are not touched."
