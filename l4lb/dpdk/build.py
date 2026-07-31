
import os 
import subprocess as sp
HERE = os.path.dirname(os.path.abspath(__file__))

def build():
    sp.run(["cmake","-S", ".","-G","Ninja","-B","./build"], cwd=HERE)
    sp.run(["ninja","-C","./build"], cwd=HERE)

if __name__ == "__main__":
    build()