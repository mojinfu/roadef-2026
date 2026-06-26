#
# Dockerfile template v1
#
# Instructions:
# - please only make modifications to this file in-between the indication flags
# - for better performance, you can remove packages/solvers not needed by your program
#
FROM ubuntu:24.04
WORKDIR /home
COPY . /home/
# used for solver installation
RUN mkdir -p /opt/hexaly
RUN mkdir -p /opt/gurobi1302
RUN mkdir -p /opt/ibm
RUN chown 1006410000:1006410000 /opt/hexaly
RUN chown 1006410000:1006410000 /opt/gurobi1302
RUN chown 1006410000:1006410000 /opt/ibm

# --------------------------------
# --- modifications start here ---
# --------------------------------

# Install python, git, and build tools in a single layer
# Add "build-essential" to get g++ and other compilers.
# Add "git" if needed.
# Finally, we clean up the apt cache to reduce image size.
RUN apt-get update && apt-get install -y \
    python3 \
    python3-pip \
    python3-venv \
    wget \
    git \
    build-essential \
    && rm -rf /var/lib/apt/lists/*

# Install python packages (make sure all required packages are listed)
# You can also use a requirements.txt file (generated with 'pip freeze > requirements.txt')
# by replacing the command below by 'pip install -r requirements.txt'
RUN python3 -m venv /opt/venv
RUN /opt/venv/bin/pip install \
    networkx \
    numpy \
    gurobipy \
    pyomo
ENV PATH="/opt/venv/bin:$PATH"

# HEXALY: install hexaly solver (remove if not needed)
RUN /opt/venv/bin/pip install -i https://pip.hexaly.com hexaly

# CPLEX: this line will trigger cplex installation later (remove if you don't need cplex)
ENV PATH="$PATH:/opt/ibm/ILOG/CPLEX_Studio2212/cplex/bin/x86-64_linux/"

# GUROBI: remove if gurobi is not used (or only with gurobipy)
ENV PATH="$PATH:/opt/gurobi1302/linux64/bin/"
ENV LD_LIBRARY_PATH="$LD_LIBRARY_PATH:/opt/gurobi1302/linux64/lib/"

# Clone a repository and compile the C++ code (remove if not needed)
RUN git clone https://gitlab.com/Orange-OpenSource/network-optimization-tools/networktools.git
RUN g++ -O3 -std=c++20 -I./networktools/networktools main.cpp -o main

# ------------------------------
# --- modifications end here ---
# ------------------------------

RUN chown -R 1006410000:1006410000 /home/
USER 1006410000
CMD ["sleep", "infinity"]
