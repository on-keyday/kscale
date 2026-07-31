
import os 
import subprocess as sp
HERE = os.path.dirname(os.path.abspath(__file__))

sp.run(["docker", "build", "--target", "artifact", "--output", "type=local,dest=./build", "."], cwd=HERE)
