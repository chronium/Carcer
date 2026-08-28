.DEFAULT_GOAL := seed

CROSS_CC := x86_64-elf-gcc
CROSS_LD := x86_64-elf-ld
HOST_CC := cc

BUILD_DIR := build/seed
ISO_ROOT := $(BUILD_DIR)/iso-root
OBJECT := $(BUILD_DIR)/kernel.o
KERNEL := $(BUILD_DIR)/kernel.elf
LIMINE_TOOL := $(BUILD_DIR)/limine
IMAGE := $(BUILD_DIR)/codexos-seed.iso
LIMINE_DIR := third_party/limine

CFLAGS := -std=c11 -O2 -Wall -Wextra -Werror -ffreestanding \
	-fno-stack-protector -fno-pic -fno-pie -fno-asynchronous-unwind-tables \
	-m64 -march=x86-64 -mno-red-zone -mno-mmx -mno-sse -mno-sse2 \
	-mcmodel=kernel
LDFLAGS := -static --build-id=none -z max-page-size=0x1000
REQUIRED_TOOLS := $(CROSS_CC) $(CROSS_LD) $(HOST_CC) xorriso install mkdir rm
REQUIRED_LIMINE_FILES := limine.c limine-bios.sys limine-bios-cd.bin

.PHONY: seed check-tools clean

seed: check-tools
	$(MAKE) $(IMAGE)

check-tools:
	@missing=0; \
	for tool in $(REQUIRED_TOOLS); do \
		if ! command -v "$$tool" >/dev/null 2>&1; then \
			echo "missing required utility: $$tool" >&2; \
			missing=1; \
		fi; \
	done; \
	test "$$missing" -eq 0
	@for file in $(REQUIRED_LIMINE_FILES); do \
		if ! test -f "$(LIMINE_DIR)/$$file"; then \
			echo "missing required Limine dependency file: $(LIMINE_DIR)/$$file" >&2; \
			echo "run git submodule update --init" >&2; \
			exit 1; \
		fi; \
	done

$(BUILD_DIR):
	mkdir -p $@

$(OBJECT): seed/kernel.c Makefile | $(BUILD_DIR)
	$(CROSS_CC) $(CFLAGS) -c $< -o $@

$(KERNEL): $(OBJECT) seed/linker.ld
	$(CROSS_LD) $(LDFLAGS) -T seed/linker.ld $< -o $@

$(LIMINE_TOOL): $(LIMINE_DIR)/limine.c | $(BUILD_DIR)
	$(HOST_CC) -std=c99 -O2 $< -o $@

$(IMAGE): $(KERNEL) $(LIMINE_TOOL) seed/limine.conf \
	$(LIMINE_DIR)/limine-bios.sys $(LIMINE_DIR)/limine-bios-cd.bin
	rm -rf $(ISO_ROOT)
	install -Dm644 $(KERNEL) $(ISO_ROOT)/boot/kernel.elf
	install -Dm644 seed/limine.conf $(ISO_ROOT)/boot/limine/limine.conf
	install -Dm644 $(LIMINE_DIR)/limine-bios.sys \
		$(ISO_ROOT)/boot/limine/limine-bios.sys
	install -Dm644 $(LIMINE_DIR)/limine-bios-cd.bin \
		$(ISO_ROOT)/boot/limine/limine-bios-cd.bin
	xorriso -as mkisofs -R -r -J \
		-V CODEXOS_SEED \
		--modification-date=2020010100000000 \
		--set_all_file_dates 2020010100000000 \
		-b boot/limine/limine-bios-cd.bin \
		-no-emul-boot -boot-load-size 4 -boot-info-table \
		$(ISO_ROOT) -o $@
	$(LIMINE_TOOL) bios-install $@
	rm -rf $(ISO_ROOT)

clean:
	rm -rf $(BUILD_DIR)
