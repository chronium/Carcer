.DEFAULT_GOAL := seed

CROSS_CC := x86_64-elf-gcc
CROSS_LD := x86_64-elf-ld
HOST_CC := cc
PYTHON := python3

BUILD_DIR := build/seed
ISO_ROOT := $(BUILD_DIR)/iso-root
SEED_SOURCES := seed/kernel.c seed/serial.c seed/protocol.c \
	seed/files.c seed/tools.c seed/build.c seed/source_snapshot.c
SEED_HEADERS := seed/serial.h seed/protocol.h seed/files.h seed/tools.h \
	seed/build.h seed/source_snapshot.h
SOURCE_TABLE_C := $(BUILD_DIR)/source-files.c
SOURCE_TABLE_OBJECT := $(BUILD_DIR)/source-files.o
OBJECTS := $(patsubst seed/%.c,$(BUILD_DIR)/%.o,$(SEED_SOURCES)) \
	$(SOURCE_TABLE_OBJECT)
SOURCE_IMAGE := $(BUILD_DIR)/source-image.o
KERNEL := $(BUILD_DIR)/kernel.elf
LIMINE_TOOL := $(BUILD_DIR)/limine
IMAGE := $(BUILD_DIR)/codexos-seed.iso
LIMINE_DIR := third_party/limine

CFLAGS := -std=c11 -O2 -Wall -Wextra -Werror -ffreestanding \
	-fno-stack-protector -fno-pic -fno-pie -fno-asynchronous-unwind-tables \
	-m64 -march=x86-64 -mno-red-zone -mno-mmx -mno-sse -mno-sse2 \
	-mcmodel=kernel
LDFLAGS := -static --build-id=none -z max-page-size=0x1000
REQUIRED_TOOLS := $(CROSS_CC) $(CROSS_LD) $(HOST_CC) $(PYTHON) \
	xorriso install mkdir rm
REQUIRED_LIMINE_FILES := limine.c limine-bios.sys limine-bios-cd.bin
GUEST_SOURCE_INPUTS := seed/build.c seed/build.h seed/files.c seed/files.h \
	seed/kernel.c seed/limine.conf seed/linker.ld seed/protocol.c \
	seed/protocol.h seed/serial.c seed/serial.h seed/source_snapshot.c \
	seed/source_snapshot.h seed/tools.c seed/tools.h

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

$(BUILD_DIR)/%.o: seed/%.c $(SEED_HEADERS) Makefile | $(BUILD_DIR)
	$(CROSS_CC) $(CFLAGS) -c $< -o $@

$(SOURCE_TABLE_C): scripts/generate_seed_source_table.py \
	Makefile $(GUEST_SOURCE_INPUTS) | $(BUILD_DIR)
	$(PYTHON) $< $@ $(GUEST_SOURCE_INPUTS)

$(SOURCE_TABLE_OBJECT): $(SOURCE_TABLE_C) seed/files.h Makefile
	$(CROSS_CC) $(CFLAGS) -Iseed -c $< -o $@

$(SOURCE_IMAGE): Makefile $(GUEST_SOURCE_INPUTS) | $(BUILD_DIR)
	$(CROSS_LD) -r -b binary $(GUEST_SOURCE_INPUTS) -o $@

$(KERNEL): $(OBJECTS) $(SOURCE_IMAGE) seed/linker.ld
	$(CROSS_LD) $(LDFLAGS) -T seed/linker.ld $(OBJECTS) $(SOURCE_IMAGE) -o $@

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
