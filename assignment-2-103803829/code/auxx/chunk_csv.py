#!/usr/bin/env python3
"""
Split CSV file into N chunks for parallel producers.
Usage: python3 chunk_csv.py <input_csv> <num_chunks>
"""

import csv
import sys
from pathlib import Path

def chunk_csv(input_file, num_chunks):
    """Split CSV file into N chunks."""
    
    input_path = Path(input_file)
    if not input_path.exists():
        print(f"Error: {input_file} not found")
        sys.exit(1)
    
    output_dir = input_path.parent / "chunks"
    output_dir.mkdir(exist_ok=True)
    
    # Read CSV file and count lines
    with open(input_file, 'r') as f:
        reader = csv.reader(f, delimiter=';')
        header = next(reader)
        rows = list(reader)
    
    total_rows = len(rows)
    chunk_size = (total_rows + num_chunks - 1) // num_chunks  # Ceiling division
    
    print(f"Total rows: {total_rows}")
    print(f"Number of chunks: {num_chunks}")
    print(f"Rows per chunk: {chunk_size}")
    print(f"Creating chunks in: {output_dir}\n")
    
    # Create chunk files
    for i in range(num_chunks):
        start_idx = i * chunk_size
        end_idx = min((i + 1) * chunk_size, total_rows)
        chunk_rows = rows[start_idx:end_idx]
        
        if not chunk_rows:
            continue
        
        chunk_file = output_dir / f"chunk_{i}.csv"
        
        with open(chunk_file, 'w', newline='') as f:
            writer = csv.writer(f, delimiter=';')
            writer.writerow(header)
            writer.writerows(chunk_rows)
        
        print(f"Created chunk_{i}.csv: rows {start_idx}-{end_idx-1} ({len(chunk_rows)} records)")
    
    print(f"\nChunking complete! Files ready in: {output_dir}")

if __name__ == "__main__":
    if len(sys.argv) != 3:
        print("Usage: python3 chunk_csv.py <input_csv> <num_chunks>")
        sys.exit(1)
    
    try:
        num_chunks = int(sys.argv[2])
        if num_chunks < 1:
            print("Error: num_chunks must be >= 1")
            sys.exit(1)
    except ValueError:
        print("Error: num_chunks must be an integer")
        sys.exit(1)
    
    chunk_csv(sys.argv[1], num_chunks)
