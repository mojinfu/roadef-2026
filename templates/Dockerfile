#
# Dockerfile template v2.1 (01/07)
#
FROM ubuntu:24.04
WORKDIR /home
COPY . /home/
RUN chmod -R +x /home/

# --------------------------------
# --- modifications start here ---
# --------------------------------

# Install python, git, and build tools in a single layer
# We add "build-essential" to get g++ and other compilers.
# We also add "git" because it's needed for the "git clone" command.
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
RUN mkdir -p /opt/hexaly
COPY _hexaly_license.dat /opt/hexaly/license.dat
RUN /opt/venv/bin/pip install -i https://pip.hexaly.com hexaly

# CPLEX: install cplex solver (remove if not needed)
COPY _build_cplex.tgz /home/_build_cplex.tgz
RUN tar -xvzf /home/_build_cplex.tgz -C /
ENV PATH="$PATH:/opt/ibm/ILOG/CPLEX_Studio2212/cplex/bin/x86-64_linux/"
ENV LD_LIBRARY_PATH="$LD_LIBRARY_PATH:/opt/ibm/ILOG/CPLEX_Studio2212/cplex/lib/x86-64_linux/static_pic/"

# GUROBI: install gurobi solver (remove if not needed)
# GUROBI: install gurobi solver (remove if not needed)
COPY _build_gurobi.tgz /home/_build_gurobi.tgz
RUN tar -xvzf /home/_build_gurobi.tgz -C /opt/
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
