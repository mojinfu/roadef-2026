#!/bin/sh
#
# run.sh template v1
#
# Instructions:
# - replace the lines after the flag to run your program
# - make sure the solution is stored in the filename given as 4th argument
#
if [ "$#" -ne 4 ]; then
  echo "Usage: $0 net tm scenar output"
  exit 1
fi

# === modifications start here
python main.py --net $1 --tm $2 --scenario $3
cp solution.json $4



