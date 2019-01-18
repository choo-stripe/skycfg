# Largely based off of https://github.com/google/starlark-go/blob/master/starlarktest/assert.star

def _eq(x, y):
    if x != y:
        fail("%r != %r" % (x, y))

def _ne(x, y):
    if x == y:
        fail("%r == %r" % (x, y))

def _true(cond, msg = "assertion failed"):
    if not cond:
        fail(msg)

def _lt(x, y):
    if not (x < y):
        fail("%s is not less than %s" % (x, y))

def _contains(x, y):
    if y not in x:
        fail("%s does not contain %s" % (x, y))

def _fails(f, pattern):
    "assert_fails asserts that evaluation of f() fails with the specified error."
    msg = catch(f)
    if msg == None:
        fail("evaluation succeeded unexpectedly (want error matching %r)" % pattern)
    elif not matches(pattern, msg):
        fail("regular expression (%s) did not match error (%s)" % (pattern, msg))

assert = struct(
    eq = _eq,
    ne = _ne,
    true = _true,
    lt = _lt,
    contains = _contains,
    fails = _fails,
)

def make_test_ctx(vars):
    return struct(vars = vars)
