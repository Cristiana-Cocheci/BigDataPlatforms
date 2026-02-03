#!/usr/bin/env python3
"""
Extract a specific day from a CSV file by hardcoded date.
Usage: python3 split_csv_by_day.py <input_csv>
"""

import csv
import sys
from pathlib import Path

# Hardcode the date to extract
TARGET_DATE = "2025-06-01"

def extract_by_date(input_file, target_date):
    """Extract only specified date from CSV file."""
    
    input_path = Path(input_file)
    if not input_path.exists():
        print(f"Error: {input_file} not found")
        sys.exit(1)
    
    output_dir = input_path.parent
    
    # Read CSV and extract target date only
    header = None
    target_data = []
    
    with open(input_file, 'r') as f:
        reader = csv.reader(f, delimiter=';')
        header = next(reader)  # Read header
        
        for row in reader:
            if len(row) < 6:  # Skip malformed rows
                continue
            
            # Extract day from timestamp (6th column, format: 2025-06-15T...)
            timestamp = row[5]
            try:
                day = timestamp.split('T')[0]  # Extract date part
                
                if day == target_date:
                    target_data.append(row)
                    
            except (IndexError, AttributeError):
                print(f"Warning: Could not parse timestamp: {timestamp}")
                continue
    
    if not target_data:
        print(f"Error: No data found for {target_date}")
        sys.exit(1)
    
    # Write output file
    output_file = output_dir / f"{target_date}_bme280.csv"
    
    with open(output_file, 'w', newline='') as f:
        writer = csv.writer(f, delimiter=';')
        writer.writerow(header)  # Write header
        writer.writerows(target_data)  # Write data
    
    print(f"Created {output_file.name}: {len(target_data)} records for {target_date}")

if __name__ == "__main__":
    if len(sys.argv) != 2:
        print("Usage: python3 split_csv_by_day.py <input_csv>")
        sys.exit(1)
    
    extract_by_date(sys.argv[1], TARGET_DATE)
