# This is a deployment/installation guide

1) Create a compatible Python 3.11 environment (PyFlink 1.20.x does not work on Python 3.14):
```
    /opt/homebrew/bin/python3.11 -m venv .venv311
    source .venv311/bin/activate
```

2) Ensure Java is available for PyFlink runtime and builds:
```
   export JAVA_HOME=/opt/homebrew/opt/openjdk@11/libexec/openjdk.jdk/Contents/Home
   export PATH="$JAVA_HOME/bin:$PATH"
  ```

3) Install dependency:
```
   pip install -U "setuptools<81" wheel
   pip install --no-build-isolation -r requirements_streamanalyticsapp.txt
```

4) Run desired test suite

```
# All tests
cd assignment-3-103803829/code
bash auxx/run_performance_tests.sh

# Only one or more tests

bash auxx/run_performance_tests.sh --only=A1_burst[,A2_moderate,...]

```

5) Observe results and logs in [results](../code/results) and [logs](../code/logs) folders.