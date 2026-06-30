# Color codes for output
RED=\033[0;31m
GREEN=\033[0;32m
YELLOW=\033[0;33m
BLUE=\033[0;34m
NC=\033[0m # No Color

# Networktools location
IDIRNT?=~/networktools

# Language selection (default: EN)
# Usage: make CHECKER_LANG=EN or make CHECKER_LANG=FR
CHECKER_LANG?=EN

# FTXUI support (default: disabled)
# Usage: make ENABLE_FTXUI=1 to enable FTXUI
ENABLE_FTXUI?=0

# FTXUI directories
FTXUI_DIR=./ftxui
FTXUI_BUILD=$(FTXUI_DIR)/build
FTXUI_INCLUDE=$(FTXUI_DIR)/include
FTXUI_LIBS=$(FTXUI_BUILD)/libftxui-dom.a $(FTXUI_BUILD)/libftxui-screen.a $(FTXUI_BUILD)/libftxui-component.a

SOURCES=main.cpp

# Extract version from config.h
VERSION=$(shell grep 'define CHECKER_VERSION' config.h | sed 's/.*"\(.*\)".*/\1/')
TARGET=checker-v$(VERSION)-x86-64_linux

CC=g++
# Base compiler flags
CFLAGS=-O3 -std=c++20 -DNDEBUG -ffunction-sections -fdata-sections -Wl,--gc-sections
# CFLAGS=-g -std=c++20 -fno-stack-protector -fsanitize=address
LDFLAGS=-static -static-libstdc++ -static-libgcc -s -Wl,--gc-sections

# Add language definition
CFLAGS+=-DLANG_$(CHECKER_LANG)

# Add FTXUI definition if enabled
ifeq ($(ENABLE_FTXUI),1)
	CFLAGS+=-DENABLE_FTXUI
	FTXUI_INCLUDE_FLAG=-I$(FTXUI_INCLUDE)
	FTXUI_LIBS_FLAG=$(FTXUI_LIBS)
	FTXUI_DEP=ftxui-build
else
	FTXUI_INCLUDE_FLAG=
	FTXUI_LIBS_FLAG=
	FTXUI_DEP=
endif


.PHONY: all clean checker ftxui-fetch ftxui-build validate-networktools

all: validate-networktools $(FTXUI_DEP) $(TARGET)

# Validate networktools installation
validate-networktools:
	@if [ ! -d "$(IDIRNT)" ]; then \
		echo "$(RED)Error: Networktools directory not found at: $(IDIRNT)$(NC)"; \
		echo "$(YELLOW)Please set IDIRNT to your networktools installation:$(NC)"; \
		echo "$(YELLOW)  make IDIRNT=/path/to/networktools$(NC)"; \
		exit 1; \
	fi
	@if [ ! -f "$(IDIRNT)/networktools.h" ]; then \
		echo "$(RED)Error: Expected header file not found: $(IDIRNT)/networktools.h$(NC)"; \
		echo "$(YELLOW)Please verify IDIRNT points to networktools directory.$(NC)"; \
		echo "$(YELLOW)Example: make IDIRNT=/path/to/networktools$(NC)"; \
		exit 1; \
	fi
	@echo "$(GREEN)✓ Networktools found at: $(IDIRNT)$(NC)"

# Fetch FTXUI if not present
ftxui-fetch:
	@if [ ! -d "$(FTXUI_DIR)" ]; then \
		echo "$(BLUE)Fetching FTXUI...$(NC)"; \
		git clone https://github.com/ArthurSonzogni/FTXUI.git $(FTXUI_DIR); \
	fi

# Build FTXUI libraries
ftxui-build: ftxui-fetch
	@if [ ! -f "$(FTXUI_BUILD)/libftxui-dom.a" ]; then \
		echo "$(BLUE)Building FTXUI...$(NC)"; \
		mkdir -p $(FTXUI_BUILD) && \
		cd $(FTXUI_BUILD) && \
		cmake .. -DCMAKE_BUILD_TYPE=Release -DFTXUI_BUILD_EXAMPLES=OFF -DFTXUI_BUILD_TESTS=OFF -DFTXUI_ENABLE_INSTALL=OFF && \
		make -j$$(nproc); \
	fi

$(TARGET): $(FTXUI_DEP) $(SOURCES) config.h
	@echo "$(BLUE)Building with: CHECKER_LANG=$(CHECKER_LANG), ENABLE_FTXUI=$(ENABLE_FTXUI)$(NC)"
	$(CC) -o $(TARGET) $(CFLAGS) $(SOURCES) -I$(IDIRNT) $(FTXUI_INCLUDE_FLAG) $(FTXUI_LIBS_FLAG) $(LDFLAGS)
	@echo "$(GREEN)✓ Built: $(TARGET) (version $(VERSION))$(NC)"

# Convenience target to build with generic name
checker: $(TARGET)
	ln -sf $(TARGET) checker
	@echo "$(GREEN)✓ Created symlink: checker -> $(TARGET)$(NC)"

clean: 
	rm -f *.o checker-v*-x86-64_linux checker

clean-all: clean
	rm -rf $(FTXUI_DIR)