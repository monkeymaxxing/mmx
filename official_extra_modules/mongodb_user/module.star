# mongodb_user manages one user of a MongoDB server: its password and its
# roles. It runs MongoDB's shell mongosh as root on the node of the server.
# docs/changing-mmx/design/modules-official-extra.md ("MongoDB") owns the behavior.
#
# mongosh runs a JavaScript program, MONGOSH_PROGRAM below, that the module
# sends through standard input with the task's values at its top. The program
# opens the connection, catches every error, and prints one line of JSON as
# its last line. The module decides from that JSON, by error code and error
# name, never by a message text.
#
# A failure that another try cannot fix is invalid_inputs, which MMX tries
# once: a rule that the inputs break, a name that MongoDB cannot take for a
# database, a login that the server refuses, a command that the server
# refuses to the login user, and a server whose replica set has another
# primary. What another try may fix is failed: a server that does not answer,
# a replica set without a primary, and a command that failed for another
# reason. A failure that another try would hide is failed with
# try_again: False: every failure of a task that created the user, and a run
# that was to create the user and ended without saying whether it did.
# What an earlier node task of the same playbook may still provide is a
# missing prerequisite, which a dryrun reports as not checked: mongosh, the
# server's first administrator, a role, and a user for a task that has no
# password to create it.

MODULE = "mongodb_user"

DEFAULT_CONNECTION_STRING = "mongodb://127.0.0.1:27017/?directConnection=true"

# With HOME=/dev/null, mongosh cannot make the directory /root/.mongodb, where
# it otherwise writes a log file for every run. That log can hold the text of
# the program, with the task's passwords in it. The command line is the same
# for every task and holds nothing of the task.
MONGOSH_COMMAND = ["env", "HOME=/dev/null", "mongosh", "--quiet", "--norc", "--nodb", "--file", "/dev/stdin"]

# Before 2.2.0, mongosh prints more than one warning and terminal codes into
# its standard output when it cannot write into HOME.
OLDEST_MONGOSH = (2, 2, 0)

# A server with access control on and no user accepts any first user, and
# refuses every further one without a login. Only a first user with one of
# these roles on the database admin can create the next user.
ROLES_THAT_MANAGE_USERS = ["root", "userAdminAnyDatabase", "userAdmin"]

# The errors of mongosh for a server that does not answer, for a connection
# string that it cannot read, and for an input that mongosh itself refuses.
ERRORS_OF_A_SERVER_THAT_DOES_NOT_ANSWER = ["MongoNetworkError", "MongoServerSelectionError"]
ERRORS_OF_A_CONNECTION_STRING = ["MongoParseError", "MongoshInvalidInputError"]
ERROR_OF_MONGOSH_FOR_AN_INPUT_THAT_IT_REFUSES = "MongoshInvalidInputError"

# A removal uses none of these inputs, and refuses a task that gives one.
INPUTS_THAT_A_REMOVAL_DOES_NOT_USE = ["password", "roles", "roles_not_listed"]

# The program that mongosh runs. The module puts one line before it:
# "const input = <the task's values as JSON>;".
#
# The text must always hold "new Mongo(" and ".auth(": mongosh leaves a
# program with one of these words out of its log. That is the second
# protection of the passwords, beside HOME=/dev/null.
MONGOSH_PROGRAM = r"""
// The input that names the database which the running step works on, and for
// a role's database the role. When mongosh or the server refuses the name of
// a database, the module names this input.
let database_named_by = {input: null, role: null};

function works_on_the_database_of(input_name, role) {
  database_named_by = {input: input_name, role: role === undefined ? null : role};
}

function describe_error(step, error) {
  return {
    step: step,
    name: String(error.name),
    code: error.code === undefined ? null : error.code,
    code_name: error.codeName === undefined ? null : String(error.codeName),
    message: String(error.message),
    database_input: database_named_by.input,
    role: database_named_by.role,
  };
}

function log_in(connection) {
  works_on_the_database_of("login_database");
  connection.getDB(input.login_database).auth(input.login_user, input.login_password);
}

function read_user(database) {
  works_on_the_database_of("database");
  const found = database.runCommand({usersInfo: {user: input.name, db: input.database}}).users;
  if (found.length === 0) {
    return null;
  }
  return {roles: found[0].roles.map(function (held) { return {role: held.role, db: held.db}; })};
}

function read() {
  const result = {};
  let step = "connect";
  try {
    const connection = new Mongo(input.mongodb_connection_string);
    const admin = connection.getDB("admin");

    // mongosh refuses a name that it cannot take for a database when it
    // opens the database, before it asks the server anything. So every name
    // of the task is opened here, whatever state the server is in.
    step = "open_database";
    works_on_the_database_of("database");
    const database = connection.getDB(input.database);
    if (input.login_user !== null) {
      works_on_the_database_of("login_database");
      connection.getDB(input.login_database);
    }
    for (const wanted of input.roles_to_look_up) {
      works_on_the_database_of("roles", wanted.role);
      connection.getDB(wanted.db);
    }

    step = "read_replica_set_state";
    const hello = admin.runCommand({hello: 1});
    result.is_primary = hello.isWritablePrimary === true;
    result.primary = typeof hello.primary === "string" ? hello.primary : "";
    result.replica_set = typeof hello.setName === "string" ? hello.setName : "";
    result.replica_set_note = typeof hello.info === "string" ? hello.info : "";
    if (!result.is_primary) {
      return result;
    }

    // Without a login, these two read commands answer differently in each
    // of the three access states. Code 13 is the server's "Unauthorized".
    step = "read_access_state";
    let lists_databases = true;
    try {
      admin.runCommand({listDatabases: 1, nameOnly: true});
    } catch (error) {
      if (error.code !== 13) { throw error; }
      lists_databases = false;
    }
    if (lists_databases) {
      try {
        result.user = read_user(database);
        result.access = "off";
      } catch (error) {
        if (error.code !== 13) { throw error; }
        result.access = "no_user_yet";
        return result;
      }
    } else {
      result.access = "login_needed";
      if (input.login_user === null) {
        return result;
      }
      // Code 18 is the server's "AuthenticationFailed".
      step = "log_in";
      try {
        log_in(connection);
      } catch (error) {
        if (error.code !== 18) { throw error; }
        result.login_refused = true;
        return result;
      }
      step = "read_user";
      result.user = read_user(database);
    }

    step = "read_roles";
    result.roles_that_do_not_exist = [];
    for (const wanted of input.roles_to_look_up) {
      works_on_the_database_of("roles", wanted.role);
      const found = connection.getDB(wanted.db).runCommand({rolesInfo: {role: wanted.role, db: wanted.db}}).roles;
      if (found.length === 0) {
        result.roles_that_do_not_exist.push(wanted);
      }
    }

    if (result.user !== null && input.password !== null) {
      // The password is the user's when the server accepts a login with it.
      // The login runs on a second connection, so that the first connection
      // keeps the login it has.
      step = "check_password";
      works_on_the_database_of("database");
      const second = new Mongo(input.mongodb_connection_string);
      try {
        second.getDB(input.database).auth(input.name, input.password);
        result.password_is_the_users = true;
      } catch (error) {
        if (error.code !== 18) { throw error; }
        result.password_is_the_users = false;
      } finally {
        second.close();
      }
    }
    return result;
  } catch (error) {
    result.error = describe_error(step, error);
    return result;
  }
}

function change() {
  const result = {done: []};
  let step = "connect";
  try {
    const connection = new Mongo(input.mongodb_connection_string);
    step = "open_database";
    works_on_the_database_of("database");
    const database = connection.getDB(input.database);
    if (input.log_in) {
      step = "log_in";
      log_in(connection);
    }
    works_on_the_database_of("database");
    for (const wanted of input.changes) {
      step = wanted.change;
      let answer;
      if (wanted.change === "create_user") {
        answer = database.createUser({user: input.name, pwd: input.password, roles: wanted.roles});
      } else if (wanted.change === "set_password") {
        answer = database.updateUser(input.name, {pwd: input.password});
      } else if (wanted.change === "set_roles") {
        answer = database.updateUser(input.name, {roles: wanted.roles});
      } else if (wanted.change === "grant_roles") {
        answer = database.grantRolesToUser(input.name, wanted.roles);
      } else if (wanted.change === "remove_user") {
        answer = database.dropUser(input.name);
      } else {
        throw new Error("the program does not know the change " + wanted.change);
      }
      if (answer.ok !== 1) {
        throw new Error("the server's answer does not say ok: 1");
      }
      result.done.push(wanted.change);
    }
    return result;
  } catch (error) {
    result.error = describe_error(step, error);
    return result;
  }
}

print(JSON.stringify(input.mode === "read" ? read() : change()));
"""

def run_node_task(inputs):
    task, problem = task_from_inputs(inputs)
    if problem:
        return {"status": "invalid_inputs", "reason": MODULE + "." + problem}

    problem = problem_with_the_programs()
    if problem:
        return problem

    found, problem = run_program(run_command, task, {
        "mode": "read",
        "password": task["password"] if task["state"] == "present" else None,
        "roles_to_look_up": task["roles"] if task["state"] == "present" and task["roles"] != None else [],
    })
    if problem:
        return failed(problem)
    if "error" in found:
        return result_of_a_failed_read(task, found["error"])
    if not found["is_primary"]:
        return result_for_a_server_that_is_not_the_primary(found)

    if found["access"] == "no_user_yet":
        return first_user_of_the_server(task)
    if found["access"] == "login_needed":
        if task["login_user"] == None:
            return {"status": "invalid_inputs", "reason": MODULE + ".login_user: this MongoDB server needs a login; give login_user and login_password"}
        if found.get("login_refused"):
            return refused_login(task)

    changes, created, problem = changes_for(task, found)
    if problem:
        return problem
    return make_changes(task, changes, created, log_in = found["access"] == "login_needed")

# task_from_inputs returns the task's values with their defaults, or the rule
# that the inputs break, as "<input>: <the rule>".
def task_from_inputs(inputs):
    task = {
        "name": inputs["name"],
        "database": inputs["database"],
        "state": inputs.get("state", "present"),
        "password": inputs.get("password"),
        "roles": None,
        "roles_not_listed": inputs.get("roles_not_listed"),
        "login_user": inputs.get("login_user"),
        "login_password": inputs.get("login_password"),
        "login_database": inputs.get("login_database", "admin"),
        "mongodb_connection_string": inputs.get("mongodb_connection_string", DEFAULT_CONNECTION_STRING),
    }
    if task["state"] == "absent":
        for name in INPUTS_THAT_A_REMOVAL_DOES_NOT_USE:
            if name in inputs:
                return None, "state: absent accepts only name, database, login_user, login_password, login_database, and mongodb_connection_string; remove " + name
    for name in ["name", "database", "password", "login_user", "login_password", "login_database", "mongodb_connection_string"]:
        if task[name] == "":
            return None, name + ": must not be empty"
    if task["login_user"] != None and task["login_password"] == None:
        return None, "login_password: is required with login_user"
    if task["login_password"] != None and task["login_user"] == None:
        return None, "login_user: is required with login_password"
    if "roles" in inputs:
        if task["roles_not_listed"] == None:
            return None, "roles_not_listed: is required with roles: remove takes every role that roles does not list away from the user, and keep leaves those roles"
        roles, problem = roles_from_input(inputs["roles"])
        if problem:
            return None, "roles: " + problem
        task["roles"] = roles
    elif task["roles_not_listed"] != None:
        return None, "roles: is required with roles_not_listed"
    if connection_string_holds_a_login(task["mongodb_connection_string"]):
        return None, "mongodb_connection_string: must hold no user name and no password; give them as login_user and login_password"
    return task, ""

# roles_from_input returns the roles as mongosh takes them, each once:
# [{"role": ..., "db": ...}].
def roles_from_input(listed):
    form = "a list of roles, each with the two keys role and database, such as {role: readWrite, database: shop}"
    if type(listed) != "list":
        return None, "expected " + form
    roles = []
    for position, entry in enumerate(listed):
        if type(entry) != "dict" or sorted(entry.keys()) != ["database", "role"]:
            return None, "entry %d: expected %s" % (position + 1, form)
        for key in ["role", "database"]:
            if type(entry[key]) != "string" or entry[key] == "":
                return None, "entry %d: %s must be a text that is not empty" % (position + 1, key)
        role = {"role": entry["role"], "db": entry["database"]}
        if role not in roles:
            roles.append(role)
    return roles, ""

# A connection string holds a login when an @ stands before its first / or ?
# behind the scheme: mongodb://user:password@host/.
def connection_string_holds_a_login(connection_string):
    _, _, behind_the_scheme = connection_string.partition("://")
    hosts = behind_the_scheme.split("/")[0].split("?")[0]
    return "@" in hosts

# problem_with_the_programs returns the result for a program that is not on
# the node, or for a mongosh that is too old, and None when both programs are
# there.
def problem_with_the_programs():
    for program, package in [("env", "coreutils"), ("mongosh", "mongodb-mongosh, from MongoDB's own package repository,")]:
        located = find_program(program)
        if located.error:
            return failed("looking for the program %s: %s" % (program, located.error))
        if not located.found:
            return {"status": "missing_prerequisite",
                    "reason": "%s: the program %s is not installed; install the package %s in an earlier node task" % (MODULE, program, package)}
    asked = run_command(["mongosh", "--version"])
    printed = asked.standard_output.strip()
    if asked.error or asked.exit_code != 0:
        return failed("mongosh --version failed: " + (asked.error or first_line(asked.standard_error) or "exit code %d" % asked.exit_code))
    version = version_numbers(printed)
    if version == None:
        return failed("mongosh --version printed %r, which the module cannot read as a version" % printed)
    if version < OLDEST_MONGOSH:
        return {"status": "missing_prerequisite",
                "reason": "%s: mongosh %s is installed, and the module needs mongosh 2.2.0 or newer; install a newer package mongodb-mongosh in an earlier node task" % (MODULE, printed)}
    return None

# version_numbers reads "2.13.0" as (2, 13, 0), and None for another text.
def version_numbers(text):
    parts = text.split("-")[0].split(".")
    if len(parts) != 3:
        return None
    for part in parts:
        if not part.isdigit():
            return None
    return (int(parts[0]), int(parts[1]), int(parts[2]))

# run_program runs mongosh with the module's program through the primitive
# run (run_command, or run_command_that_changes_the_node) and returns what the
# program printed as its last line. The program ends with exit code 0 whenever
# it printed its result. In a dryrun the changing primitive runs nothing, and
# the result is None.
def run_program(run, task, values):
    data = {
        "mongodb_connection_string": task["mongodb_connection_string"],
        "name": task["name"],
        "database": task["database"],
        "password": task["password"],
        "login_user": task["login_user"],
        "login_password": task["login_password"],
        "login_database": task["login_database"],
    }
    data.update(values)
    out = run(MONGOSH_COMMAND, standard_input = "const input = " + json.encode(data) + ";\n" + MONGOSH_PROGRAM)
    if out.error:
        return None, "running mongosh: " + out.error
    if not out.ran:
        return None, ""
    lines = [line for line in out.standard_output.split("\n") if line.strip()]
    printed = json.decode(lines[-1], default = None) if lines else None
    if out.exit_code != 0 or type(printed) != "dict":
        # Only the first line of standard error: mongosh prints the text of a
        # program that it cannot read behind it.
        return None, "mongosh ended with exit code %d and without a result that the module can read: %s" % (out.exit_code, first_line(out.standard_error))
    return printed, ""

def first_line(text):
    lines = [line for line in text.split("\n") if line.strip()]
    return lines[0].strip() if lines else ""

def failed(reason):
    return {"status": "failed", "reason": MODULE + ": " + reason}

# no_password_to_create_the_user is the result for a user that the server
# does not have and a task without password. A playbook can have two tasks
# for one user: the first creates it, and the second, without password,
# changes its roles. In a dryrun the first one has created nothing yet, so
# this is a missing prerequisite and no rejected input.
def no_password_to_create_the_user(task):
    return {"status": "missing_prerequisite",
            "reason": "%s: %s does not exist, and the task has no password to create it; add password, or create the user in an earlier mongodb_user task" %
                      (MODULE, user_text(task))}

# failed_after_a_change is the failed result of a changing run. When the
# task created the user before it failed, MMX must not try again: the next
# try would find the user and report created: false for what this task
# created.
def failed_after_a_change(reason, created_the_user):
    result = {"status": "failed", "reason": reason, "outputs": {"created": created_the_user}}
    if created_the_user:
        result["try_again"] = False
    return result

# refused_login is the result when the server refuses the login of
# login_user. Another try gets the same answer.
def refused_login(task):
    return {"status": "invalid_inputs",
            "reason": "%s: the MongoDB server refused the login of the user %s in the database %s; check login_user, login_database, and login_password" %
                      (MODULE, task["login_user"], task["login_database"])}

# The server's error 13, "Unauthorized", for a command of a connection that
# is logged in: the login user is not allowed to run it, now or at another
# try.
def refused_to_the_login_user(error, logged_in):
    return logged_in and error["code"] == 13

# refused_database_name returns the result for a name that MongoDB cannot
# take for a database, and None for another error. mongosh refuses such a
# name when it opens the database, with its error for an input that it
# refuses. The server refuses one with its error 73, "InvalidNamespace". The
# program says which input named the database.
def refused_database_name(error):
    by_mongosh = error["step"] == "open_database" and error["name"] == ERROR_OF_MONGOSH_FOR_AN_INPUT_THAT_IT_REFUSES
    if error["database_input"] == None or not (by_mongosh or error["code"] == 73):
        return None
    if error["database_input"] == "roles":
        return {"status": "invalid_inputs",
                "reason": "%s.roles: MongoDB cannot take the database of the role %s as the name of a database: %s" % (MODULE, error["role"], error_text(error))}
    return {"status": "invalid_inputs",
            "reason": "%s.%s: MongoDB cannot take this as the name of a database: %s" % (MODULE, error["database_input"], error_text(error))}

def user_text(task):
    return "the user %s in the database %s" % (task["name"], task["database"])

# roles_text names roles as "the role read on the database reports" or "the
# roles readWrite on the database shop, read on the database reports".
def roles_text(roles):
    names = ", ".join(["%s on the database %s" % (role["role"], role["db"]) for role in roles])
    return ("the role " if len(roles) == 1 else "the roles ") + names

# error_text is the message of an error of mongosh or of the server, with the
# server's number and name for the error where it has them.
def error_text(error):
    if error["code_name"]:
        return "%s (MongoDB's error %s, %s)" % (error["message"], error["code"], error["code_name"])
    return error["message"]

# result_of_a_failed_read turns an error of the reading program into the
# task's result.
def result_of_a_failed_read(task, error):
    if error["name"] in ERRORS_OF_A_SERVER_THAT_DOES_NOT_ANSWER:
        # An earlier node task of the same run may start the server.
        return {"status": "not_checked" if is_dryrun() else "failed",
                "reason": "%s: the MongoDB server does not answer: %s" % (MODULE, error_text(error))}
    if error["step"] == "connect" and error["name"] in ERRORS_OF_A_CONNECTION_STRING:
        return {"status": "invalid_inputs",
                "reason": "%s.mongodb_connection_string: mongosh cannot read this connection string: %s" % (MODULE, error_text(error))}
    refused = refused_database_name(error)
    if refused:
        return refused
    tried = {
        "connect": "connecting to the MongoDB server",
        "open_database": "opening the databases that the task names",
        "read_replica_set_state": "asking the MongoDB server whether it is the primary of a replica set",
        "read_access_state": "asking the MongoDB server whether it needs a login",
        "log_in": "logging in as the user %s in the database %s" % (task["login_user"], task["login_database"]),
        "read_user": "reading " + user_text(task),
        "read_roles": "reading the roles that the task lists",
        "check_password": "checking the password of " + user_text(task),
    }[error["step"]]
    # The program reads the user and the roles only after its login, where
    # the server needs one.
    if refused_to_the_login_user(error, task["login_user"] != None):
        return {"status": "invalid_inputs",
                "reason": "%s: %s: %s; login_user must be a user that is allowed to manage users" % (MODULE, tried, error_text(error))}
    return failed("%s: %s" % (tried, error_text(error)))

# result_for_a_server_that_is_not_the_primary is for a member of a replica
# set that cannot change users.
def result_for_a_server_that_is_not_the_primary(found):
    the_set = "the replica set " + found["replica_set"] if found["replica_set"] else "a replica set"
    start = "%s: this MongoDB server is a member of %s and not its primary, and only the primary changes users. " % (MODULE, the_set)
    if found["primary"]:
        # Another try reaches the same server again.
        return {"status": "invalid_inputs",
                "reason": start + ("The primary is %s: run this task on that node, or put replicaSet=%s into mongodb_connection_string " +
                                   "and leave directConnection=true out, so that mongosh goes to the primary by itself") % (found["primary"], found["replica_set"])}

    # A replica set chooses its primary a few seconds after an earlier node
    # task initiated it, and after its members started: another try may find
    # one, and a dryrun cannot know.
    note = " (%s)" % found["replica_set_note"] if found["replica_set_note"] else ""
    return {"status": "not_checked" if is_dryrun() else "failed", "reason": start + "The replica set has no primary now" + note}

# first_user_of_the_server handles a server with access control on and no
# user yet. Only the first administrator can be created there, without a
# login, in the database admin.
def first_user_of_the_server(task):
    if task["state"] != "present" or task["database"] != "admin":
        return {"status": "missing_prerequisite",
                "reason": MODULE + ": this MongoDB server has access control on and no administrator yet; " +
                          "create the first user in the database admin, with the role root, in an earlier mongodb_user task"}
    situation = "this MongoDB server has access control on and no user yet, so this task creates its first user. "
    roles = task["roles"] or []
    elsewhere = [role for role in roles if role["db"] != "admin"]
    if elsewhere:
        return {"status": "invalid_inputs",
                "reason": MODULE + ".roles: " + situation + "MongoDB lets the first user hold only roles on the database admin, and the task lists " +
                          roles_text(elsewhere) + ". Give the first user the role root on admin"}
    if not [role for role in roles if role["role"] in ROLES_THAT_MANAGE_USERS]:
        return {"status": "invalid_inputs",
                "reason": MODULE + ".roles: " + situation + "The first user must be allowed to manage users: " +
                          "give it the role root, userAdminAnyDatabase, or userAdmin on the database admin. " +
                          "MongoDB would accept a first user without such a role, and nobody could create a user afterwards"}
    if task["password"] == None:
        return no_password_to_create_the_user(task)
    return make_changes(task, [{"change": "create_user", "roles": roles, "as_first_user": True}], True, log_in = False)

# changes_for compares the user that the server has with the task and returns
# the changes that a run makes, in order, and whether the task creates the
# user. A problem is a whole result.
def changes_for(task, found):
    user = found["user"]
    if task["state"] == "absent":
        return ([] if user == None else [{"change": "remove_user"}]), False, None
    if user == None and task["password"] == None:
        return None, False, no_password_to_create_the_user(task)
    if found["roles_that_do_not_exist"]:
        missing = found["roles_that_do_not_exist"]
        return None, False, {"status": "missing_prerequisite",
                             "reason": "%s: %s %s not exist on this MongoDB server; create %s in an earlier node task" %
                                       (MODULE, roles_text(missing), "does" if len(missing) == 1 else "do", "it" if len(missing) == 1 else "them")}
    if user == None:
        return [{"change": "create_user", "roles": task["roles"] or []}], True, None

    changes = []
    if task["password"] != None and not found["password_is_the_users"]:
        changes.append({"change": "set_password"})
    if task["roles"] != None:
        # The server gives the roles in no fixed order: compare them as sets.
        to_grant = [role for role in task["roles"] if role not in user["roles"]]
        to_revoke = sorted([role for role in user["roles"] if role not in task["roles"]], key = lambda role: (role["db"], role["role"]))
        if task["roles_not_listed"] == "remove" and (to_grant or to_revoke):
            # One command writes the whole list.
            changes.append({"change": "set_roles", "roles": task["roles"], "granted": to_grant, "revoked": to_revoke})
        elif task["roles_not_listed"] == "keep" and to_grant:
            changes.append({"change": "grant_roles", "roles": to_grant})
    return changes, False, None

# what_a_change_does returns one entry for each thing that a change does: the
# verb for a run's reason, for a dryrun's reason, and for a failure's reason,
# and the thing. Writing a whole list of roles grants some and revokes others.
def what_a_change_does(task, change):
    kind = change["change"]
    if kind == "create_user":
        roles = change["roles"]
        text = user_text(task) + (" with " + roles_text(roles) if roles else " without a role")
        if change.get("as_first_user"):
            text += ", as the first user of this MongoDB server"
        return [("created", "would create", "creating", text)]
    if kind == "set_password":
        return [("set", "would set", "setting", "the password of " + user_text(task))]
    if kind == "grant_roles":
        return [("granted", "would grant", "granting", roles_text(change["roles"]) + " to " + user_text(task))]
    if kind == "set_roles":
        sentences = []
        if change["granted"]:
            sentences.append(("granted", "would grant", "granting", roles_text(change["granted"]) + " to " + user_text(task)))
        if change["revoked"]:
            sentences.append(("revoked", "would revoke", "revoking", roles_text(change["revoked"]) + " from " + user_text(task)))
        return sentences
    return [("removed", "would remove", "removing", user_text(task))]

# make_changes runs the changes with one mongosh, and returns the task's
# result. A dryrun runs nothing and says what a run would do.
def make_changes(task, changes, created, log_in):
    result = {"outputs": {"created": created}, "fingerprint": fingerprint(task)}
    if not changes:
        return result
    sent = [{"change": change["change"], "roles": change.get("roles", [])} for change in changes]
    answer, problem = run_program(run_command_that_changes_the_node, task, {"mode": "change", "log_in": log_in, "changes": sent})
    if problem:
        return result_after_a_change_with_unknown_outcome(task, changes, created, problem)
    if answer == None:
        result["reason"] = "; ".join([sentence[1] + " " + sentence[3] for change in changes for sentence in what_a_change_does(task, change)])
        return result

    done = [sentence[0] + " " + sentence[3] for change in changes[:len(answer["done"])] for sentence in what_a_change_does(task, change)]
    if "error" in answer:
        error = answer["error"]
        # Code 18 is the server's "AuthenticationFailed".
        if error["step"] == "log_in" and error["code"] == 18:
            return refused_login(task)
        refused = refused_database_name(error)
        if refused:
            return refused
        tried = {
            "connect": "connecting to the MongoDB server",
            "open_database": "opening the database " + task["database"],
            "log_in": "logging in as the user %s in the database %s" % (task["login_user"], task["login_database"]),
        }.get(error["step"])
        if tried == None:
            tried = " and ".join([sentence[2] + " " + sentence[3] for sentence in what_a_change_does(task, changes[len(answer["done"])])])
        if error["step"] == "create_user" and error["code"] == None:
            # Neither the server nor mongosh refused the command: the
            # connection ended, and the answer did not arrive.
            return result_after_a_change_with_unknown_outcome(task, changes, created, "%s: %s" % (tried, error_text(error)))
        before = ". Before this failure, the task " + "; ".join(done) if done else ""
        advice = "; login_user must be a user that is allowed to manage users" if refused_to_the_login_user(error, log_in) else ""
        reason = "%s: %s: %s%s%s" % (MODULE, tried, error_text(error), advice, before)
        if advice or error["name"] == ERROR_OF_MONGOSH_FOR_AN_INPUT_THAT_IT_REFUSES:
            # Another try gets the same refusal.
            return {"status": "invalid_inputs", "reason": reason}
        return failed_after_a_change(reason, created and len(done) > 0)
    result["reason"] = "; ".join(done)
    return result

# result_after_a_change_with_unknown_outcome is for a changing run that ended
# without saying what it did: mongosh failed, or the connection ended before
# the server's answer to the creation arrived.
#
# A run that only changes or removes a user is tried again: the next try
# reads the server and does what is still missing. A run that was to create
# the user is different. The server may have created the user, and the next
# try would then find it, change nothing, and report created: false, so that
# nobody reads what failed. The module therefore reads the server once more.
# Where the user is there, or the module cannot find out, the task fails
# without another try.
def result_after_a_change_with_unknown_outcome(task, changes, created, what_failed):
    if not created:
        return failed(what_failed)
    creating = " and ".join([sentence[2] + " " + sentence[3] for sentence in what_a_change_does(task, changes[0])])
    if not what_failed.startswith(creating):
        what_failed = creating + ": " + what_failed
    found, problem = run_program(run_command, task, {"mode": "read", "password": None, "roles_to_look_up": []})
    knows = not problem and "error" not in found and found.get("is_primary") == True
    if knows and (found.get("access") == "no_user_yet" or ("user" in found and found["user"] == None)):
        # The user is not there: the next try creates it.
        return failed(what_failed)
    if knows and found.get("user") != None:
        return failed_after_a_change("%s: %s. The server has %s now: it created the user before this failure, and the next run compares the user with the task" %
                                     (MODULE, what_failed, user_text(task)), True)
    return {"status": "failed", "try_again": False,
            "reason": "%s: %s. The module could not find out whether the server created %s; the next run compares the user with the task" %
                      (MODULE, what_failed, user_text(task))}

# The fingerprint is the state that the task asks for, without a password.
def fingerprint(task):
    roles = None
    if task["roles"] != None:
        roles = sorted(["%s on %s" % (role["role"], role["db"]) for role in task["roles"]])
    return json.encode({"name": task["name"], "database": task["database"], "state": task["state"],
                        "roles": roles, "roles_not_listed": task["roles_not_listed"]})
